package config

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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

// Manager 持有当前生效快照。读取走原子指针，永不加锁；
// 重载互斥串行，只有全部来源加载并解析成功后才原子替换，
// 因此并发调用方不可能观察到半更新状态，失败时上一份快照原样保留。
type Manager struct {
	engine  *Engine
	sources []Source

	mu       sync.Mutex // 串行化重载
	revision int64
	seenIDs  map[string]struct{}

	current atomic.Pointer[Snapshot]
}

// NewManager 创建管理器；sources 顺序即优先级从低到高。
func NewManager(engine *Engine, sources ...Source) *Manager {
	return &Manager{
		engine:  engine,
		sources: sources,
		seenIDs: map[string]struct{}{},
	}
}

// LoadInitial 做首次加载，成功后版本号为 1；已初始化后再次调用会被拒绝。
func (m *Manager) LoadInitial(ctx context.Context) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision != 0 {
		return nil, fmt.Errorf("config: manager already initialized at revision %d", m.revision)
	}
	return m.buildLocked(ctx)
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
	if req.RequestID != "" {
		if _, dup := m.seenIDs[req.RequestID]; dup {
			return nil, &DuplicateReloadError{RequestID: req.RequestID}
		}
		m.seenIDs[req.RequestID] = struct{}{}
	}
	return m.buildLocked(ctx)
}

// buildLocked 在持有 m.mu 的前提下执行加载与解析；
// 任何一步失败都不触碰 current，旧快照继续可用。
func (m *Manager) buildLocked(ctx context.Context) (*Snapshot, error) {
	layers := make([]RawLayer, 0, len(m.sources))
	for _, src := range m.sources {
		data, err := src.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("reload failed at revision %d (source %q); previous config retained: %w",
				m.revision, src.Name(), err)
		}
		layers = append(layers, RawLayer{Name: src.Name(), Data: data})
	}

	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		return nil, fmt.Errorf("reload failed at revision %d; previous config retained: %w",
			m.revision, err)
	}

	resolved.Revision = m.revision + 1
	m.revision = resolved.Revision
	snap := *resolved
	m.current.Store(&snap)
	return &snap, nil
}

// Current 返回当前生效快照；尚未初始化时返回 nil。
func (m *Manager) Current() *Snapshot { return m.current.Load() }
