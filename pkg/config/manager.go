package config

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Manager 管理配置的加载、原子重载与查询。
// 查询无锁；重载串行化，任何时刻调用方只能看到完整的旧快照或新快照。
type Manager struct {
	schema  *Schema
	sources []Source

	mu      sync.Mutex // 串行化 Reload
	cur     atomic.Pointer[snapshot]
	version uint64 // 已生效快照版本，仅 Reload 持锁时修改
}

// NewManager 构造管理器：校验 Schema，随后立即执行一次初始加载。
// 初始加载失败时返回错误（此时没有可用快照）。
func NewManager(ctx context.Context, schema *Schema, sources ...Source) (*Manager, error) {
	if schema == nil {
		return nil, errors.New("config: schema is nil")
	}
	if err := schema.validate(); err != nil {
		return nil, err
	}
	m := &Manager{schema: schema, sources: append([]Source(nil), sources...)}
	snap, err := build(ctx, schema, m.sources, 1)
	if err != nil {
		return nil, err
	}
	m.cur.Store(snap)
	m.version = 1
	return m, nil
}

// Reload 重新加载所有来源并原子替换当前快照。
// expectVersion 为调用方期望的当前版本：
//   - 传 0 表示无条件重载；
//   - 传非 0 且与当前版本不一致时，返回 ReasonStaleReload，不产生任何变更。
//
// 重载失败时保留上一份可用快照，并返回可区分原因的错误。
func (m *Manager) Reload(ctx context.Context, expectVersion uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.cur.Load()
	if expectVersion != 0 && cur != nil && expectVersion != cur.version {
		return cur.version, errf(ReasonStaleReload, "", "",
			"stale reload: expect version %d, current %d", expectVersion, cur.version)
	}

	next := uint64(1)
	if cur != nil {
		next = cur.version + 1
	}
	snap, err := build(ctx, m.schema, m.sources, next)
	if err != nil {
		if cur == nil {
			return 0, err
		}
		return cur.version, err
	}
	m.cur.Store(snap)
	m.version = snap.version
	return snap.version, nil
}

// Version 返回当前生效快照版本。
func (m *Manager) Version() uint64 {
	if s := m.cur.Load(); s != nil {
		return s.version
	}
	return 0
}

// Get 返回字段当前生效值。
func (m *Manager) Get(path string) (string, bool) {
	s := m.cur.Load()
	if s == nil {
		return "", false
	}
	r, ok := s.vals[canonical(path)]
	return r.value, ok
}

// Explain 返回字段当前生效值与其来源，用于可解释性。
func (m *Manager) Explain(path string) (value, source string, ok bool) {
	s := m.cur.Load()
	if s == nil {
		return "", "", false
	}
	r, ok := s.vals[canonical(path)]
	return r.value, r.source, ok
}

// Warnings 返回最近一次成功构建产生的告警（弃用、迁移提示等）。
func (m *Manager) Warnings() []Warning {
	s := m.cur.Load()
	if s == nil {
		return nil
	}
	return append([]Warning(nil), s.warns...)
}

// Decode 将当前快照填充到 out（结构体指针）。
func (m *Manager) Decode(out any) error {
	s := m.cur.Load()
	if s == nil {
		return errors.New("config: no snapshot available")
	}
	return decodeInto(s.vals, out)
}
