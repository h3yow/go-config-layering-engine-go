package config

// View 是某一版配置快照的不可变一致视图。
// 一次 Snapshot() 调用完成单次原子加载；View 内所有字段属于同一版本，
// 即使在此期间发生重载，View 的内容也不会变化，因此调用方永远不会
// 通过同一个 View 观察到半更新状态。
type View struct {
	version uint64
	vals    map[string]resolved
	warns   []Warning
}

// Version 返回该视图对应的快照版本。
func (v *View) Version() uint64 { return v.version }

// Get 返回字段值。
func (v *View) Get(path string) (string, bool) {
	r, ok := v.vals[canonical(path)]
	return r.value, ok
}

// Explain 返回字段值与其来源。
func (v *View) Explain(path string) (value, source string, ok bool) {
	r, ok := v.vals[canonical(path)]
	return r.value, r.source, ok
}

// Warnings 返回构建该快照时产生的告警。
func (v *View) Warnings() []Warning {
	return append([]Warning(nil), v.warns...)
}

// Decode 将该视图填充到 out（结构体指针）。
func (v *View) Decode(out any) error {
	return decodeInto(v.vals, out)
}

// Snapshot 返回当前快照的一致不可变视图（单次原子加载，无锁）。
// 尚未成功加载过配置时返回 nil。
func (m *Manager) Snapshot() *View {
	s := m.cur.Load()
	if s == nil {
		return nil
	}
	return &View{version: s.version, vals: s.vals, warns: s.warns}
}
