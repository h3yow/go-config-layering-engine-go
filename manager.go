package config

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot 是对外暴露的某一版配置，与 Resolved 相同。
type Snapshot = Resolved

// ReloadRequest 携带并发控制信息：基于的版本与去重请求 ID。
//   - ExpectedRevision：调用方认为当前生效的版本号，不匹配即判定为过期请求；
//   - RequestID：非空时作为幂等键，重复提交被拒绝。
type ReloadRequest struct {
	ExpectedRevision int64
	RequestID        string
}

// maxRememberedRequestIDs 是去重请求 ID 的保留上限（FIFO 淘汰），
// 保证持续高频请求下内存占用有界。
const maxRememberedRequestIDs = 1024

// requestIDSet 是有界的请求 ID 去重集：容量固定，超出后按先进先出淘汰最旧 ID。
type requestIDSet struct {
	set   map[string]struct{}
	order []string
	max   int
}

func newRequestIDSet(max int) *requestIDSet {
	return &requestIDSet{set: map[string]struct{}{}, max: max}
}

// add 记录 ID；已存在（未被淘汰）时返回 false 表示重复。
func (s *requestIDSet) add(id string) bool {
	if _, dup := s.set[id]; dup {
		return false
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.set, oldest)
	}
	return true
}

func (s *requestIDSet) len() int { return len(s.order) }

// Manager 持有当前生效快照与版本历史。读取走原子指针，永不加锁；
// 重载/发布/回滚互斥串行，只有全部来源加载并解析成功后才原子替换，
// 因此并发调用方不可能观察到半更新状态，失败时上一份快照原样保留。
type Manager struct {
	engine  *Engine
	sources []Source

	mu           sync.Mutex // 串行化一切写操作（重载/发布/回滚/历史调整）
	revision     int64
	seen         *requestIDSet
	history      []VersionRecord // 按版本从旧到新
	historyLimit int
	evictedUpTo  int64 // 已被淘汰的最大版本号（含），0 表示尚未淘汰过

	current atomic.Pointer[Snapshot]
}

// NewManager 创建管理器；sources 顺序即优先级从低到高。
// 版本历史保留上限为 DefaultHistoryLimit，可用 SetHistoryLimit 调整。
func NewManager(engine *Engine, sources ...Source) *Manager {
	return &Manager{
		engine:       engine,
		sources:      sources,
		seen:         newRequestIDSet(maxRememberedRequestIDs),
		historyLimit: DefaultHistoryLimit,
	}
}

// LoadInitial 做首次加载，成功后版本号为 1；已初始化后再次调用会被拒绝。
func (m *Manager) LoadInitial(ctx context.Context) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision != 0 {
		return nil, fmt.Errorf("config: manager already initialized at revision %d", m.revision)
	}
	layers, _, err := m.loadLayersLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("initial load failed: %w", err)
	}
	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		return nil, fmt.Errorf("initial load failed: %w", err)
	}
	return m.commitLocked(resolved, VersionInitial, ""), nil
}

// Reload 重新读取所有来源并原子替换快照：
//   - 期望版本不匹配返回 *StaleReloadError；
//   - 重复 RequestID 返回 *DuplicateReloadError；
//   - 加载或解析失败时保留上一份可用配置，并把失败原因原样返回。
func (m *Manager) Reload(ctx context.Context, req ReloadRequest) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.revision == 0 {
		return nil, fmt.Errorf("config: manager not initialized; call LoadInitial first")
	}
	if req.ExpectedRevision != m.revision {
		return nil, &StaleReloadError{Expected: req.ExpectedRevision, Current: m.revision}
	}
	if err := m.checkRequestIDLocked(req.RequestID); err != nil {
		return nil, err
	}

	layers, _, err := m.loadLayersLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reload failed at revision %d; previous config retained: %w",
			m.revision, err)
	}
	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		return nil, fmt.Errorf("reload failed at revision %d; previous config retained: %w",
			m.revision, err)
	}
	return m.commitLocked(resolved, VersionReload, ""), nil
}

// loadLayersLocked 在持有 m.mu 的前提下加载全部来源，
// 返回原始层与来源内容指纹（用于预演/发布一致性校验）。
func (m *Manager) loadLayersLocked(ctx context.Context) ([]RawLayer, string, error) {
	layers := make([]RawLayer, 0, len(m.sources))
	for _, src := range m.sources {
		data, err := src.Load(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("source %q: %w", src.Name(), err)
		}
		layers = append(layers, RawLayer{Name: src.Name(), Data: data})
	}
	return layers, fingerprintLayers(layers), nil
}

// commitLocked 在持有 m.mu 的前提下把解析结果原子生效：
// 递增版本号、替换快照、记录版本历史并按上限淘汰最旧版本。
func (m *Manager) commitLocked(resolved *Resolved, kind VersionKind, note string) *Snapshot {
	resolved.Revision = m.revision + 1
	m.revision = resolved.Revision
	snap := *resolved
	m.current.Store(&snap)
	m.recordLocked(&snap, kind, note)
	return &snap
}

// recordLocked 追加一条版本历史并按保留上限淘汰最旧记录。
// 当前生效版本永远是最新一条，不会被淘汰。
func (m *Manager) recordLocked(snap *Snapshot, kind VersionKind, note string) {
	m.history = append(m.history, VersionRecord{
		Revision:    snap.Revision,
		Kind:        kind,
		PublishedAt: time.Now(),
		Note:        note,
		Snapshot:    snap,
	})
	m.evictLocked()
}

// evictLocked 淘汰超出保留上限的最旧版本记录。
// 淘汰只是移除历史引用：快照不可变，已持有引用的读取方不受影响，
// 但被丢弃的版本号之后不能再作为回滚目标。
func (m *Manager) evictLocked() {
	for len(m.history) > m.historyLimit {
		m.evictedUpTo = m.history[0].Revision
		m.history[0] = VersionRecord{} // 释放引用
		m.history = m.history[1:]
	}
}

// checkRequestIDLocked 校验并登记幂等请求 ID；重复时返回 *DuplicateReloadError。
func (m *Manager) checkRequestIDLocked(id string) error {
	if id == "" {
		return nil
	}
	if !m.seen.add(id) {
		return &DuplicateReloadError{RequestID: id}
	}
	return nil
}

// findVersionLocked 在保留中的历史里查找指定版本。
func (m *Manager) findVersionLocked(revision int64) (*VersionRecord, bool) {
	for i := range m.history {
		if m.history[i].Revision == revision {
			return &m.history[i], true
		}
	}
	return nil, false
}

// Current 返回当前生效快照；尚未初始化时返回 nil。
func (m *Manager) Current() *Snapshot { return m.current.Load() }
