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

// 历史与去重记录的保留上限默认值。三者都是有界常量，保证持续高频
// 发布/回滚下内存占用稳定。
const (
	defaultMaxHistory     = 16
	maxPendingPreviews    = 32
	seenRequestIDCapacity = 4096
)

// ManagerOption 定制 Manager 行为。
type ManagerOption func(*Manager)

// WithMaxHistory 设置版本历史的保留上限（>=1）；超出后淘汰最旧的版本。
// 未设置时默认 16。
func WithMaxHistory(n int) ManagerOption {
	return func(m *Manager) {
		if n >= 1 {
			m.maxHistory = n
		}
	}
}

// Manager 持有当前生效快照。读取走原子指针，永不加锁；
// 重载/发布/回滚互斥串行，只有全部来源加载并解析成功后才原子替换，
// 因此并发调用方不可能观察到半更新状态，失败时上一份快照原样保留。
type Manager struct {
	engine  *Engine
	sources []Source

	mu       sync.Mutex // 串行化一切写路径（初始加载/重载/发布/回滚）
	revision int64

	// 请求去重：map + FIFO 队列组成有界集合，容量 seenRequestIDCapacity。
	seenIDs   map[string]struct{}
	seenOrder []string

	// 版本历史：按版本号从旧到新，长度不超过 maxHistory。
	maxHistory int
	history    []VersionRecord

	// 待发布预演：按创建先后淘汰，数量不超过 maxPendingPreviews。
	previews   map[string]*Preview
	previewSeq int64
	previewOrd []string

	current atomic.Pointer[Snapshot]
}

// NewManager 创建管理器；sources 顺序即优先级从低到高。
// schema 中声明的默认值会自动作为最低优先级的一层（名为 "defaults"）
// 参与合并，无需调用方再手工提供。
func NewManager(engine *Engine, sources ...Source) *Manager {
	return NewManagerWithOptions(engine, nil, sources...)
}

// NewManagerWithOptions 创建带选项的管理器。
func NewManagerWithOptions(engine *Engine, opts []ManagerOption, sources ...Source) *Manager {
	m := &Manager{
		engine:     engine,
		sources:    sources,
		seenIDs:    map[string]struct{}{},
		maxHistory: defaultMaxHistory,
		previews:   map[string]*Preview{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// LoadInitial 做首次加载，成功后版本号为 1；已初始化后再次调用会被拒绝。
func (m *Manager) LoadInitial(ctx context.Context) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision != 0 {
		return nil, fmt.Errorf("config: manager already initialized at revision %d", m.revision)
	}
	return m.buildLocked(ctx, VersionInitial, "initial load")
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
	if err := m.claimRequestIDLocked(req.RequestID); err != nil {
		return nil, err
	}
	return m.buildLocked(ctx, VersionReload, "reload")
}

// buildLocked 在持有 m.mu 的前提下执行加载与解析；
// 任何一步失败都不触碰 current，旧快照继续可用。
// 成功后记录版本历史，并使基于旧版本的待发布预演失效。
func (m *Manager) buildLocked(ctx context.Context, kind VersionKind, note string) (*Snapshot, error) {
	layers, err := m.loadLayersLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("reload failed at revision %d; previous config retained: %w",
			m.revision, err)
	}

	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		return nil, fmt.Errorf("reload failed at revision %d; previous config retained: %w",
			m.revision, err)
	}

	return m.activateLocked(resolved, kind, note), nil
}

// loadLayersLocked 读取全部来源；schema 默认值作为最低优先级的一层前置。
func (m *Manager) loadLayersLocked(ctx context.Context) ([]RawLayer, error) {
	layers := make([]RawLayer, 0, len(m.sources)+1)
	if defs := defaultLayer(m.engine.schema); len(defs) > 0 {
		layers = append(layers, RawLayer{Name: "defaults", Data: defs})
	}
	for _, src := range m.sources {
		data, err := src.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.Name(), err)
		}
		layers = append(layers, RawLayer{Name: src.Name(), Data: data})
	}
	return layers, nil
}

// activateLocked 把解析结果作为新版本原子生效并记入历史。
// 基于旧版本的预演继续保留（数量有上限），正式发布时会被
// StalePreviewError 明确拒绝，提示调用方重新预演。
func (m *Manager) activateLocked(resolved *Resolved, kind VersionKind, note string) *Snapshot {
	snap := *resolved
	snap.Revision = m.revision + 1
	m.revision = snap.Revision
	m.current.Store(&snap)
	m.recordLocked(kind, note, &snap)
	return &snap
}

// recordLocked 追加一条版本历史并按上限淘汰最旧记录。
// 被淘汰的只是历史列表中的位置；Snapshot 本身不可变，
// 已持有它的读者由 GC 保证永远读到完整数据。
func (m *Manager) recordLocked(kind VersionKind, note string, snap *Snapshot) {
	m.history = append(m.history, VersionRecord{
		Revision:    snap.Revision,
		Kind:        kind,
		PublishedAt: time.Now(),
		Note:        note,
		Snapshot:    snap,
	})
	for len(m.history) > m.maxHistory {
		copy(m.history, m.history[1:])
		m.history = m.history[:len(m.history)-1]
	}
}

// claimRequestIDLocked 把请求 ID 登记进有界去重集合；重复时返回
// *DuplicateReloadError。超出容量时淘汰最早登记的 ID。
func (m *Manager) claimRequestIDLocked(id string) error {
	if id == "" {
		return nil
	}
	if _, dup := m.seenIDs[id]; dup {
		return &DuplicateReloadError{RequestID: id}
	}
	m.seenIDs[id] = struct{}{}
	m.seenOrder = append(m.seenOrder, id)
	for len(m.seenOrder) > seenRequestIDCapacity {
		delete(m.seenIDs, m.seenOrder[0])
		m.seenOrder = m.seenOrder[1:]
	}
	return nil
}

// registerPreviewLocked 登记一个预演结果并返回；超出待发布上限时
// 淘汰最早创建的预演。
func (m *Manager) registerPreviewLocked(pv *Preview) *Preview {
	m.previewSeq++
	pv.ID = fmt.Sprintf("pv-%d", m.previewSeq)
	m.previews[pv.ID] = pv
	m.previewOrd = append(m.previewOrd, pv.ID)
	for len(m.previewOrd) > maxPendingPreviews {
		delete(m.previews, m.previewOrd[0])
		m.previewOrd = m.previewOrd[1:]
	}
	return pv
}

// Current 返回当前生效快照；尚未初始化时返回 nil。
func (m *Manager) Current() *Snapshot { return m.current.Load() }
