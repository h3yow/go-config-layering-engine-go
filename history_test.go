package config

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// publishPort 走一遍"改覆盖层 -> 预演 -> 发布"把端口发布为指定值。
func publishPort(t *testing.T, m *Manager, ov *OverrideSource, port int64, tag string) *Snapshot {
	t.Helper()
	cur := m.Current()
	ov.Set(RawConfig{"server": map[string]any{"port": port}})
	pv, err := m.Preview(context.Background(), PreviewRequest{
		ExpectedRevision: cur.Revision, RequestID: "pv-" + tag})
	if err != nil {
		t.Fatalf("preview %s: %v", tag, err)
	}
	if !pv.Accepted {
		t.Fatalf("preview %s rejected: %+v", tag, pv.Rejections)
	}
	snap, err := m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: cur.Revision, RequestID: "pub-" + tag})
	if err != nil {
		t.Fatalf("publish %s: %v", tag, err)
	}
	return snap
}

// 回滚成功：运行中的配置切回目标版本内容，每个值的生效来源可解释，
// 回滚本身产生新版本并记入历史。
func TestRollbackRestoresVersionWithSources(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	first, err := m.LoadInitial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publishPort(t, m, ov, 10001, "a")
	publishPort(t, m, ov, 10002, "b")
	if m.Current().Revision != 3 {
		t.Fatalf("setup: want revision 3, got %d", m.Current().Revision)
	}

	snap, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 1, ExpectedRevision: 3, RequestID: "rb-1"})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if snap.Revision != 4 {
		t.Fatalf("rollback must allocate a new revision, got %d", snap.Revision)
	}
	// 内容与来源解释与 rev1 完全一致（含 defaults 层的默认值）。
	for _, p := range []string{"app.name", "server.host", "server.port", "server.tls", "log.level"} {
		if snap.Values[p] != first.Values[p] {
			t.Errorf("%s: rolled back value %v != rev1 value %v", p, snap.Values[p], first.Values[p])
		}
		if snap.Sources[p] != first.Sources[p] {
			t.Errorf("%s: rolled back source %q != rev1 source %q", p, snap.Sources[p], first.Sources[p])
		}
	}
	hist := m.History(0)
	if hist[0].Kind != VersionRollback || hist[0].Revision != 4 {
		t.Fatalf("rollback record missing at history head: %+v", hist[0])
	}
	t.Logf("rollback to rev1 content applied as rev4; sources: %s", mustJSON(snap.Sources))
}

// 过期与重复的回滚请求被明确拒绝，当前配置保持不变。
func TestRollbackStaleAndDuplicateRejected(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishPort(t, m, ov, 10001, "a")

	_, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 1, ExpectedRevision: 99, RequestID: "rb-stale"})
	var stale *StaleReloadError
	if !errors.As(err, &stale) {
		t.Fatalf("expected StaleReloadError, got %v", err)
	}

	if _, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 1, ExpectedRevision: 2, RequestID: "rb-dup"}); err != nil {
		t.Fatalf("first rollback should succeed: %v", err)
	}
	_, err = m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 1, ExpectedRevision: 3, RequestID: "rb-dup"})
	var dup *DuplicateReloadError
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateReloadError, got %v", err)
	}
	if m.Current().Revision != 3 {
		t.Fatalf("rejected rollbacks must not advance revision: %d", m.Current().Revision)
	}
	t.Logf("stale/duplicate rollback rejected; current retained at rev %d", m.Current().Revision)
}

// 回滚目标从未存在 -> UnknownRevisionError；回滚失败保留当前配置。
func TestRollbackUnknownTargetRetainsCurrent(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishPort(t, m, ov, 10001, "a")

	_, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 42, ExpectedRevision: 2, RequestID: "rb-unknown"})
	var unk *UnknownRevisionError
	if !errors.As(err, &unk) || unk.Revision != 42 {
		t.Fatalf("expected UnknownRevisionError{42}, got %v", err)
	}
	cur := m.Current()
	if cur.Revision != 2 || cur.Values["server.port"] != int64(10001) {
		t.Fatalf("failed rollback must retain current: rev=%d port=%v",
			cur.Revision, cur.Values["server.port"])
	}
	t.Logf("unknown rollback target rejected: %v; current retained at rev %d", err, cur.Revision)
}

// 历史淘汰边界：超出保留上限后最旧版本被淘汰，回滚到已淘汰版本
// 被 EvictedRevisionError 明确拒绝；回滚到保留中的版本仍然可用。
func TestHistoryEvictionBoundary(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 3, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 连续发布 4 版（rev2..rev5），上限 3 => 最终保留 rev3,rev4,rev5。
	for i, p := range []int64{10001, 10002, 10003, 10004} {
		publishPort(t, m, ov, p, fmt.Sprintf("ev-%d", i))
	}
	hist := m.History(0)
	if len(hist) != 3 || hist[0].Revision != 5 || hist[2].Revision != 3 {
		t.Fatalf("history should retain newest 3 (rev3..5): %+v", hist)
	}
	t.Logf("history after eviction: %s", mustJSON(hist))

	// 回滚到已淘汰的 rev2 -> 明确拒绝。
	_, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 2, ExpectedRevision: 5, RequestID: "rb-evicted"})
	var ev *EvictedRevisionError
	if !errors.As(err, &ev) || ev.Revision != 2 || ev.OldestRetained != 3 {
		t.Fatalf("expected EvictedRevisionError{2,3}, got %v", err)
	}
	t.Logf("evicted rollback target rejected: %v", err)

	// 回滚到保留中的最旧版本 rev3 -> 成功。
	snap, err := m.Rollback(context.Background(), RollbackRequest{
		TargetRevision: 3, ExpectedRevision: 5, RequestID: "rb-oldest"})
	if err != nil {
		t.Fatalf("rollback to oldest retained revision: %v", err)
	}
	if snap.Values["server.port"] != int64(10002) {
		t.Fatalf("rollback to rev3 should restore port 10002, got %v", snap.Values["server.port"])
	}
	t.Logf("rollback to oldest retained rev3 ok, new revision %d", snap.Revision)
}

// 被淘汰的版本不影响已持有它的读者：快照不可变，淘汰后仍完整可读。
func TestEvictedSnapshotRemainsReadable(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 2, releaseBase(), ov)
	first, err := m.LoadInitial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []int64{10001, 10002, 10003} {
		publishPort(t, m, ov, p, fmt.Sprintf("rd-%d", i))
	}
	// rev1 早已被淘汰，但持有的快照必须仍然完整。
	if first.Values["server.port"] != int64(8080) || first.Sources["server.port"] != "defaults" ||
		first.Values["app.name"] != "demo" {
		t.Fatalf("evicted snapshot corrupted: port=%v src=%v name=%v",
			first.Values["server.port"], first.Sources["server.port"], first.Values["app.name"])
	}
	t.Logf("evicted snapshot still fully readable: port=%v source=%s",
		first.Values["server.port"], first.Sources["server.port"])
}

// 历史列表：按版本从新到旧，limit 生效，记录包含产生方式。
func TestHistoryListingOrderAndLimit(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishPort(t, m, ov, 10001, "a")
	if _, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 2, RequestID: "rl"}); err != nil {
		t.Fatal(err)
	}
	hist := m.History(0)
	if len(hist) != 3 || hist[0].Revision != 3 || hist[1].Revision != 2 || hist[2].Revision != 1 {
		t.Fatalf("history must be newest-first: %+v", hist)
	}
	if hist[0].Kind != VersionReload || hist[1].Kind != VersionPublish || hist[2].Kind != VersionInitial {
		t.Fatalf("history kinds wrong: %+v", hist)
	}
	if limited := m.History(2); len(limited) != 2 || limited[0].Revision != 3 {
		t.Fatalf("history limit broken: %+v", limited)
	}
	t.Logf("history listing: %s", mustJSON(hist))
}

// 高频发布/回滚压力：历史、待发布预演、去重集合都必须有界，
// 版本号持续单调前进，最终配置完整一致。
func TestHighFrequencyPublishRollbackStability(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 4, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	const cycles = 2000
	for i := 0; i < cycles; i++ {
		publishPort(t, m, ov, int64(10000+i%5000), fmt.Sprintf("hf-%d", i))
		// 每两轮回滚一次到保留中的某个版本。
		if i%2 == 1 {
			cur := m.Current()
			target := cur.Revision - 2
			if target < 1 {
				target = 1
			}
			if _, err := m.Rollback(context.Background(), RollbackRequest{
				TargetRevision:   target,
				ExpectedRevision: cur.Revision,
				RequestID:        fmt.Sprintf("hf-rb-%d", i),
			}); err != nil {
				t.Fatalf("cycle %d rollback to %d: %v", i, target, err)
			}
		}
	}

	if got := len(m.history); got > 4 {
		t.Fatalf("history unbounded: %d", got)
	}
	if got := len(m.previews); got > maxPendingPreviews {
		t.Fatalf("previews unbounded: %d", got)
	}
	if got := len(m.seenIDs); got > seenRequestIDCapacity {
		t.Fatalf("request id set unbounded: %d", got)
	}
	cur := m.Current()
	if cur.Values["server.port"] == nil || cur.Sources["server.port"] == "" {
		t.Fatalf("final snapshot incomplete: %+v", cur)
	}
	t.Logf("after %d cycles: revision=%d history=%d previews=%d seenIDs=%d (all bounded)",
		cycles, cur.Revision, len(m.history), len(m.previews), len(m.seenIDs))
}
