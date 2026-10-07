package config

import (
	"context"
	"fmt"
	"time"
)

// VersionKind 标识一版历史记录的产生方式。
type VersionKind string

const (
	VersionInitial  VersionKind = "initial"
	VersionReload   VersionKind = "reload"
	VersionPublish  VersionKind = "publish"
	VersionRollback VersionKind = "rollback"
)

// VersionRecord 是一次成功生效的版本记录。Snapshot 不可变，
// 即使该记录后来被淘汰，已持有它的读者也不受影响。
type VersionRecord struct {
	Revision    int64
	Kind        VersionKind
	PublishedAt time.Time
	Note        string
	Snapshot    *Snapshot
}

// RollbackRequest 请求把运行中的配置切回某个历史版本。
//   - TargetRevision：要回到的版本号（必须仍在保留的历史中）；
//   - ExpectedRevision：调用方认为当前生效的版本号；
//   - RequestID：非空时作为幂等键，重复提交被拒绝。
type RollbackRequest struct {
	TargetRevision   int64
	ExpectedRevision int64
	RequestID        string
}

// History 按版本号从新到旧列出保留中的版本记录；limit>0 时最多返回 limit 条。
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

// Rollback 把当前配置原子切回 TargetRevision 对应的内容：
// 回滚本身产生一个递增的新版本并记入历史；目标版本不存在或已被淘汰、
// 请求过期或重复时明确拒绝，当前生效配置保持不变。
func (m *Manager) Rollback(ctx context.Context, req RollbackRequest) (*Snapshot, error) {
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

	var target *VersionRecord
	for i := range m.history {
		if m.history[i].Revision == req.TargetRevision {
			target = &m.history[i]
			break
		}
	}
	if target == nil {
		if req.TargetRevision >= 1 && req.TargetRevision < m.history[0].Revision {
			return nil, &EvictedRevisionError{Revision: req.TargetRevision, OldestRetained: m.history[0].Revision}
		}
		return nil, &UnknownRevisionError{Revision: req.TargetRevision}
	}

	// 历史快照不可变：浅拷贝共享其 Values/Sources/Tree 是安全的，
	// 只替换版本号为回滚产生的新版本。
	snap := *target.Snapshot
	return m.activateLocked(&snap, VersionRollback,
		fmt.Sprintf("rollback to revision %d", req.TargetRevision)), nil
}
