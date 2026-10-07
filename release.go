package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// DefaultHistoryLimit 是版本历史的默认保留上限。
const DefaultHistoryLimit = 16

// VersionKind 标识一条历史版本记录的产生方式。
type VersionKind string

const (
	VersionInitial  VersionKind = "initial"
	VersionReload   VersionKind = "reload"
	VersionPublish  VersionKind = "publish"
	VersionRollback VersionKind = "rollback"
)

// VersionRecord 是一次成功生效（初始加载/重载/发布/回滚）留下的版本记录。
// Snapshot 不可变，调用方可安全持有引用；被淘汰的版本只是不再可回滚，
// 已持有引用的读取方不受影响。
type VersionRecord struct {
	Revision    int64
	Kind        VersionKind
	PublishedAt time.Time
	Note        string
	Snapshot    *Snapshot
}

// Rejection 是预演判定候选不可接受的一条原因，定位到字段与来源层。
type Rejection struct {
	Kind    ErrorKind
	Path    string
	Source  string
	Message string
}

// Preview 是一次发布前预演的完整结论。候选不可接受时
// Acceptable=false 且 Rejections 给出逐条原因，Candidate/Diff 为空。
// Fingerprint 唯一标识预演时读到的来源内容，正式发布会据此核对。
type Preview struct {
	BaseRevision int64
	Fingerprint  string
	Acceptable   bool
	Candidate    *Resolved
	Diff         []Change
	Rejections   []Rejection
	Warnings     []Warning
	PreviewedAt  time.Time
}

// PublishRequest 是正式发布请求：必须携带预演结论，
// 发布时会重新加载来源并校验指纹，确保发布的就是预演过的内容。
type PublishRequest struct {
	ExpectedRevision int64
	RequestID        string
	Preview          *Preview
}

// RollbackRequest 是回滚请求：把运行中的配置切回 TargetRevision 的内容，
// 生效后产生一个新的版本号（内容等同目标版本）。
type RollbackRequest struct {
	ExpectedRevision int64
	TargetRevision   int64
	RequestID        string
}

// Preview 在不影响当前生效配置的前提下，完整加载并校验候选配置，
// 与当前生效配置逐路径对比，给出整体是否可接受的结论。
//
// 配置问题（加载失败、非法取值、缺必填、类型不符、未知字段等）不会以
// error 返回，而是记入 Preview.Rejections 并将 Acceptable 置为 false——
// 与正常加载同样严格，绝不静默兜底。仅当管理器尚未初始化时返回 error。
func (m *Manager) Preview(ctx context.Context) (*Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision == 0 {
		return nil, fmt.Errorf("config: manager not initialized; call LoadInitial first")
	}

	p := &Preview{
		BaseRevision: m.revision,
		PreviewedAt:  time.Now(),
	}
	layers, fp, err := m.loadLayersLocked(ctx)
	if err != nil {
		p.Rejections = rejectionsFromErr(err)
		return p, nil
	}
	p.Fingerprint = fp

	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		p.Rejections = rejectionsFromErr(err)
		return p, nil
	}
	p.Acceptable = true
	p.Candidate = resolved
	p.Warnings = resolved.Warnings
	p.Diff = DiffResolved(m.current.Load(), resolved)
	return p, nil
}

// Publish 正式发布。预演结论必须仍然成立，否则拒绝并保留当前配置：
//   - 未携带预演或预演本身不可接受 → *PreviewMismatchError；
//   - 预演基线版本落后于当前版本 → *PreviewMismatchError；
//   - 预演后来源内容发生变化（指纹不一致）→ *PreviewMismatchError；
//   - 期望版本过期 → *StaleReloadError；重复请求 ID → *DuplicateReloadError。
//
// 通过后原子替换当前快照并记录一条 VersionPublish 历史。
func (m *Manager) Publish(ctx context.Context, req PublishRequest) (*Snapshot, error) {
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
	if req.Preview == nil {
		return nil, &PreviewMismatchError{
			BaseRevision: -1, CurrentRevision: m.revision,
			Detail: "publish requires a Preview obtained from Manager.Preview",
		}
	}
	if !req.Preview.Acceptable {
		return nil, &PreviewMismatchError{
			BaseRevision: req.Preview.BaseRevision, CurrentRevision: m.revision,
			Detail: "preview candidate was rejected; fix the config and preview again",
		}
	}
	if req.Preview.BaseRevision != m.revision {
		return nil, &PreviewMismatchError{
			BaseRevision: req.Preview.BaseRevision, CurrentRevision: m.revision,
			Detail: "current revision moved since preview; preview again before publishing",
		}
	}

	layers, fp, err := m.loadLayersLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish failed at revision %d; previous config retained: %w",
			m.revision, err)
	}
	if fp != req.Preview.Fingerprint {
		return nil, &PreviewMismatchError{
			BaseRevision: req.Preview.BaseRevision, CurrentRevision: m.revision,
			Detail: "source contents changed between preview and publish (fingerprint mismatch)",
		}
	}
	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		return nil, fmt.Errorf("publish failed at revision %d; previous config retained: %w",
			m.revision, err)
	}
	return m.commitLocked(resolved, VersionPublish, ""), nil
}

// Rollback 把运行中的配置原子切回历史中某一版的内容：
//   - 目标版本不存在 / 已被保留上限淘汰 / 就是当前版本 → *VersionNotFoundError；
//   - 期望版本过期 → *StaleReloadError；重复请求 ID → *DuplicateReloadError。
//
// 回滚不重新读取来源，直接复用历史记录中不可变的快照内容，
// 因此不会因当前来源已损坏而失败；生效后产生新版本号并记录
// VersionRollback 历史，Sources 溯源信息完整保留。
func (m *Manager) Rollback(ctx context.Context, req RollbackRequest) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.revision == 0 {
		return nil, fmt.Errorf("config: manager not initialized; call LoadInitial first")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.ExpectedRevision != m.revision {
		return nil, &StaleReloadError{Expected: req.ExpectedRevision, Current: m.revision}
	}
	if err := m.checkRequestIDLocked(req.RequestID); err != nil {
		return nil, err
	}
	if req.TargetRevision == m.revision {
		return nil, &VersionNotFoundError{Revision: req.TargetRevision, Reason: "current"}
	}
	rec, ok := m.findVersionLocked(req.TargetRevision)
	if !ok {
		reason := "unknown"
		if req.TargetRevision >= 1 && req.TargetRevision <= m.evictedUpTo {
			reason = "evicted"
		}
		return nil, &VersionNotFoundError{Revision: req.TargetRevision, Reason: reason}
	}

	// 复用历史快照的不可变内容，赋予新版本号后原子生效。
	restored := *rec.Snapshot
	snap := m.commitLocked(&restored, VersionRollback,
		fmt.Sprintf("rolled back to revision %d", req.TargetRevision))
	return snap, nil
}

// History 按版本从新到旧列出保留中的历史记录；limit<=0 时返回全部。
// 返回的是记录副本，调用方修改不影响内部状态。
func (m *Manager) History(limit int) []VersionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.history)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]VersionRecord, 0, n)
	for i := len(m.history) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, m.history[i])
	}
	return out
}

// SetHistoryLimit 调整版本历史保留上限（小于 1 时按 1 处理），
// 超出部分立即淘汰最旧记录；当前生效版本始终保留。
func (m *Manager) SetHistoryLimit(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n < 1 {
		n = 1
	}
	m.historyLimit = n
	m.evictLocked()
}

// fingerprintLayers 计算来源内容指纹：层名 + 规范化 JSON 的 SHA-256。
// encoding/json 对 map 键排序，同一来源内容必然得到同一指纹。
func fingerprintLayers(layers []RawLayer) string {
	h := sha256.New()
	for _, l := range layers {
		h.Write([]byte(l.Name))
		h.Write([]byte{0})
		data, err := json.Marshal(l.Data)
		if err != nil {
			// RawConfig 只含 JSON 风格类型，不会发生；兜底用 fmt 保证确定性。
			data = []byte(fmt.Sprintf("%#v", l.Data))
		}
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rejectionsFromErr 把解析/加载错误转成可机读的逐条拒绝原因。
func rejectionsFromErr(err error) []Rejection {
	if re, ok := AsResolutionError(err); ok {
		return []Rejection{{
			Kind:    re.Kind,
			Path:    re.Path,
			Source:  re.Source,
			Message: re.Message,
		}}
	}
	return []Rejection{{Message: err.Error()}}
}
