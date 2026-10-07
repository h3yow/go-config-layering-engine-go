package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// releaseSchema 在公共测试 schema 之上增加一个无默认值、非必填的
// 可选字段 cache.ttl，用于构造"删除"类差异。
func releaseSchema(t *testing.T) *Schema {
	t.Helper()
	base := testSchema(t)
	fields := append([]Field{}, base.Fields...)
	fields = append(fields, Field{Path: "cache.ttl", Type: TypeInt})
	s, err := NewSchema("app-release", fields)
	if err != nil {
		t.Fatalf("build release schema: %v", err)
	}
	return s
}

// newReleaseManager 构建 defaults < file < override 三层来源的管理器。
func newReleaseManager(t *testing.T, fileCfg RawConfig, ov *OverrideSource) *Manager {
	t.Helper()
	schema := releaseSchema(t)
	eng, err := NewEngine(schema, testRules())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return NewManager(eng,
		NewStaticSource("defaults", defaultLayer(schema)),
		NewStaticSource("file", fileCfg),
		ov,
	)
}

// logPreview 按要求在单测日志中打印预演输入、差异结论与判定依据。
func logPreview(t *testing.T, title string, input RawConfig, p *Preview) {
	t.Helper()
	var b strings.Builder
	b.WriteString("PREVIEW: " + title + "\n")
	if input != nil {
		b.WriteString(fmt.Sprintf("  candidate input[source=override]: %s\n", mustJSON(input)))
	}
	b.WriteString(fmt.Sprintf("  base revision: %d, fingerprint: %.12s...\n", p.BaseRevision, p.Fingerprint))
	if !p.Acceptable {
		b.WriteString("  verdict: REJECTED\n")
		for _, r := range p.Rejections {
			b.WriteString(fmt.Sprintf("  rejection: kind=%s path=%s source=%s detail=%s\n",
				r.Kind, r.Path, r.Source, r.Message))
		}
	} else {
		b.WriteString("  verdict: ACCEPTABLE\n")
		b.WriteString("  diff vs current (path: old@oldSource -> new@newSource):\n")
		for _, c := range p.Diff {
			b.WriteString(fmt.Sprintf("    [%s] %s: %v@%s -> %v@%s\n",
				c.Kind, c.Path, c.OldValue, c.OldSource, c.NewValue, c.NewSource))
		}
	}
	t.Log("\n" + b.String())
}

func logHistory(t *testing.T, title string, recs []VersionRecord) {
	t.Helper()
	var b strings.Builder
	b.WriteString("HISTORY: " + title + "\n")
	for _, r := range recs {
		note := ""
		if r.Note != "" {
			note = " (" + r.Note + ")"
		}
		b.WriteString(fmt.Sprintf("  rev=%d kind=%s%s\n", r.Revision, r.Kind, note))
	}
	t.Log("\n" + b.String())
}

// 预演成功：差异逐条给出新增/变更/删除及各自来源；默认值不丢；
// 预演不影响当前生效配置。
func TestPreviewAcceptableShowsDiffAndSources(t *testing.T) {
	ov := NewOverrideSource("override", RawConfig{
		"cache":    map[string]any{"ttl": int64(30)},
		"features": []any{"a", "b"},
	})
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatalf("initial: %v", err)
	}

	candidate := RawConfig{
		"server": map[string]any{"port": int64(20000)},
		"log":    map[string]any{"level": "debug"},
	}
	ov.Set(candidate)
	p, err := m.Preview(context.Background())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	logPreview(t, "override changes port/level, drops cache.ttl and features", candidate, p)

	if !p.Acceptable {
		t.Fatalf("expected acceptable preview, rejections: %+v", p.Rejections)
	}
	if p.BaseRevision != 1 || p.Fingerprint == "" {
		t.Fatalf("preview missing base revision or fingerprint: %+v", p)
	}

	byPath := map[string]Change{}
	for _, c := range p.Diff {
		byPath[c.Path] = c
	}
	// 变更：server.port 9090(file) -> 20000(override)
	if c, ok := byPath["server.port"]; !ok || c.Kind != ChangeChanged ||
		c.OldValue != int64(9090) || c.NewValue != int64(20000) ||
		c.OldSource != "file" || c.NewSource != "override" {
		t.Fatalf("server.port diff wrong: %+v", byPath["server.port"])
	}
	// 变更：log.level info(file) -> debug(override)
	if c, ok := byPath["log.level"]; !ok || c.Kind != ChangeChanged ||
		c.OldSource != "file" || c.NewSource != "override" {
		t.Fatalf("log.level diff wrong: %+v", byPath["log.level"])
	}
	// 删除：cache.ttl 只存在于 override，候选不再提供
	if c, ok := byPath["cache.ttl"]; !ok || c.Kind != ChangeRemoved || c.OldSource != "override" {
		t.Fatalf("cache.ttl diff wrong: %+v", byPath["cache.ttl"])
	}
	// 回落：features [a b](override) -> [](defaults)
	if c, ok := byPath["features"]; !ok || c.Kind != ChangeChanged ||
		c.OldSource != "override" || c.NewSource != "defaults" {
		t.Fatalf("features diff wrong: %+v", byPath["features"])
	}

	// 候选完整性：默认值与来源溯源不丢。
	if p.Candidate.Values["server.tls"] != false || p.Candidate.Sources["server.tls"] != "defaults" {
		t.Fatalf("default server.tls missing in candidate: %+v", p.Candidate.Values)
	}
	if p.Candidate.Values["server.host"] != "127.0.0.1" || p.Candidate.Sources["server.host"] != "file" {
		t.Fatalf("file value missing in candidate")
	}

	// 预演不得影响当前生效配置。
	cur := m.Current()
	if cur.Revision != 1 || cur.Values["server.port"] != int64(9090) {
		t.Fatalf("preview mutated current config: rev=%d port=%v", cur.Revision, cur.Values["server.port"])
	}
}

// 预演失败：非法取值、缺必填、未知字段都要给出字段/来源/原因，
// 且当前配置保持不变。
func TestPreviewRejectedExplainsFieldAndSource(t *testing.T) {
	t.Run("invalid value", func(t *testing.T) {
		ov := NewOverrideSource("override", nil)
		m := newReleaseManager(t, validFileCfg(9090), ov)
		if _, err := m.LoadInitial(context.Background()); err != nil {
			t.Fatal(err)
		}
		bad := RawConfig{"server": map[string]any{"port": int64(-5)}}
		ov.Set(bad)
		p, err := m.Preview(context.Background())
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		logPreview(t, "port out of range", bad, p)
		if p.Acceptable || len(p.Rejections) != 1 {
			t.Fatalf("expected single rejection, got %+v", p)
		}
		r := p.Rejections[0]
		if r.Kind != KindInvalidValue || r.Path != "server.port" || r.Source != "override" {
			t.Fatalf("rejection not localized: %+v", r)
		}
		if m.Current().Revision != 1 {
			t.Fatal("rejected preview must not change current revision")
		}
	})

	t.Run("missing required", func(t *testing.T) {
		ov := NewOverrideSource("override", nil)
		noName := RawConfig{"server": map[string]any{"host": "127.0.0.1", "port": int64(9090)}}
		m := newReleaseManager(t, noName, ov)
		// 首次加载即缺必填：直接失败，无法初始化。
		if _, err := m.LoadInitial(context.Background()); err == nil {
			t.Fatal("expected initial load to fail on missing required field")
		}
		var re *ResolutionError
		if _, err := m.LoadInitial(context.Background()); !errors.As(err, &re) || re.Kind != KindMissingRequired {
			t.Fatalf("expected KindMissingRequired, got %v", err)
		}
		t.Logf("missing required rejected at load: kind=%s path=%s detail=%s", re.Kind, re.Path, re.Message)
	})

	t.Run("unknown field", func(t *testing.T) {
		ov := NewOverrideSource("override", nil)
		m := newReleaseManager(t, validFileCfg(9090), ov)
		if _, err := m.LoadInitial(context.Background()); err != nil {
			t.Fatal(err)
		}
		bad := RawConfig{"bogus": map[string]any{"field": "x"}}
		ov.Set(bad)
		p, err := m.Preview(context.Background())
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		logPreview(t, "unknown field in override", bad, p)
		if p.Acceptable || len(p.Rejections) != 1 {
			t.Fatalf("expected single rejection, got %+v", p)
		}
		r := p.Rejections[0]
		if r.Kind != KindUnknownField || r.Path != "bogus.field" || r.Source != "override" {
			t.Fatalf("rejection not localized: %+v", r)
		}
	})
}

// 正式发布成功：发布内容与预演候选一致，历史留下 publish 记录。
func TestPublishSuccessMatchesPreview(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})

	p, err := m.Preview(context.Background())
	if err != nil || !p.Acceptable {
		t.Fatalf("preview: %v %+v", err, p)
	}
	logPreview(t, "publish candidate", nil, p)

	snap, err := m.Publish(context.Background(), PublishRequest{
		ExpectedRevision: p.BaseRevision,
		RequestID:        "pub-1",
		Preview:          p,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if snap.Revision != 2 || snap.Values["server.port"] != int64(20000) {
		t.Fatalf("unexpected published snapshot: rev=%d port=%v", snap.Revision, snap.Values["server.port"])
	}
	// 发布内容与预演候选逐值一致。
	if diff := DiffResolved(p.Candidate, snap); len(diff) != 0 {
		t.Fatalf("published content differs from previewed candidate: %+v", diff)
	}
	hist := m.History(0)
	logHistory(t, "after first publish", hist)
	if len(hist) != 2 || hist[0].Kind != VersionPublish || hist[0].Revision != 2 {
		t.Fatalf("history missing publish record: %+v", hist)
	}
}

// 预演与发布之间来源内容变化：指纹不一致，发布被拒绝，当前配置保留。
func TestPublishRejectsChangedSources(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})
	p, err := m.Preview(context.Background())
	if err != nil || !p.Acceptable {
		t.Fatalf("preview: %v %+v", err, p)
	}

	// 预演后来源内容被改动。
	ov.Set(RawConfig{"server": map[string]any{"port": int64(30000)}})
	_, err = m.Publish(context.Background(), PublishRequest{
		ExpectedRevision: p.BaseRevision,
		RequestID:        "pub-drift",
		Preview:          p,
	})
	var mm *PreviewMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("expected PreviewMismatchError, got %v", err)
	}
	t.Logf("publish rejected after source drift: %v", err)
	cur := m.Current()
	if cur.Revision != 1 || cur.Values["server.port"] != int64(9090) {
		t.Fatalf("current config must be retained after mismatch: %+v", cur.Values)
	}
}

// 预演基线版本落后：期间发生了一次成功重载，旧预演不得再发布。
func TestPublishRejectsStaleBase(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})
	p, err := m.Preview(context.Background())
	if err != nil || !p.Acceptable {
		t.Fatalf("preview: %v %+v", err, p)
	}
	// 另一通道先重载成功，版本前进到 2。
	if _, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "r"}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, err = m.Publish(context.Background(), PublishRequest{
		ExpectedRevision: 2,
		RequestID:        "pub-stale",
		Preview:          p,
	})
	var mm *PreviewMismatchError
	if !errors.As(err, &mm) || mm.BaseRevision != 1 || mm.CurrentRevision != 2 {
		t.Fatalf("expected PreviewMismatchError{1,2}, got %v", err)
	}
	t.Logf("stale preview rejected: %v", err)
}

// 未携带预演、或预演本身不可接受的发布请求都要被拒绝。
func TestPublishRequiresAcceptablePreview(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := m.Publish(context.Background(), PublishRequest{ExpectedRevision: 1, RequestID: "no-preview"})
	var mm *PreviewMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("expected PreviewMismatchError for nil preview, got %v", err)
	}
	t.Logf("nil preview rejected: %v", err)

	ov.Set(RawConfig{"server": map[string]any{"port": int64(-1)}})
	p, err := m.Preview(context.Background())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Acceptable {
		t.Fatal("expected rejected preview")
	}
	_, err = m.Publish(context.Background(), PublishRequest{ExpectedRevision: 1, RequestID: "bad-preview", Preview: p})
	if !errors.As(err, &mm) {
		t.Fatalf("expected PreviewMismatchError for rejected preview, got %v", err)
	}
	t.Logf("unacceptable preview rejected at publish: %v", err)
	if m.Current().Revision != 1 {
		t.Fatal("failed publishes must not advance revision")
	}
}

// 回滚成功：切回目标版本内容，产生新版本号，来源溯源完整，历史留痕。
func TestRollbackRestoresVersionContent(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	first, err := m.LoadInitial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})
	p, err := m.Preview(context.Background())
	if err != nil || !p.Acceptable {
		t.Fatalf("preview: %v %+v", err, p)
	}
	if _, err := m.Publish(context.Background(), PublishRequest{
		ExpectedRevision: 1, RequestID: "pub-1", Preview: p,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	snap, err := m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 2, TargetRevision: 1, RequestID: "rb-1",
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if snap.Revision != 3 {
		t.Fatalf("rollback must produce a new revision, got %d", snap.Revision)
	}
	// 内容与 rev1 完全一致（含默认值与来源溯源）。
	if diff := DiffResolved(first, snap); len(diff) != 0 {
		t.Fatalf("rolled back content differs from revision 1: %+v", diff)
	}
	if snap.Values["server.port"] != int64(9090) || snap.Sources["server.port"] != "file" {
		t.Fatalf("rollback content/source wrong: port=%v source=%s",
			snap.Values["server.port"], snap.Sources["server.port"])
	}
	if m.Current().Revision != 3 {
		t.Fatal("current not switched to rolled back snapshot")
	}
	hist := m.History(0)
	logHistory(t, "after rollback to rev1", hist)
	if hist[0].Kind != VersionRollback || !strings.Contains(hist[0].Note, "revision 1") {
		t.Fatalf("rollback record missing: %+v", hist[0])
	}
}

// 回滚目标校验：已淘汰、从未存在、就是当前版本，都要明确拒绝且保留现状。
func TestRollbackRejectsEvictedUnknownCurrent(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetHistoryLimit(2)
	// 前进到 rev3，rev1 被淘汰。
	for i, port := range []int64{20000, 21000} {
		ov.Set(RawConfig{"server": map[string]any{"port": port}})
		if _, err := m.Reload(context.Background(), ReloadRequest{
			ExpectedRevision: int64(i + 1), RequestID: fmt.Sprintf("r-%d", i),
		}); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	logHistory(t, "limit=2 after 3 revisions", m.History(0))

	cur := m.Current().Revision
	cases := []struct {
		target int64
		reason string
	}{
		{1, "evicted"},
		{99, "unknown"},
		{cur, "current"},
	}
	for _, tc := range cases {
		_, err := m.Rollback(context.Background(), RollbackRequest{
			ExpectedRevision: cur, TargetRevision: tc.target,
			RequestID: fmt.Sprintf("rb-%d", tc.target),
		})
		var nf *VersionNotFoundError
		if !errors.As(err, &nf) || nf.Reason != tc.reason {
			t.Fatalf("target %d: expected VersionNotFoundError(%s), got %v", tc.target, tc.reason, err)
		}
		t.Logf("rollback to %d rejected as %q: %v", tc.target, tc.reason, err)
	}
	if m.Current().Revision != cur || m.Current().Values["server.port"] != int64(21000) {
		t.Fatal("rejected rollbacks must retain current config")
	}
}

// 回滚的过期与重复请求：stale 版本与重复 RequestID 都要明确拒绝。
func TestRollbackStaleAndDuplicateRequests(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})
	if _, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}

	_, err := m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 1, TargetRevision: 1, RequestID: "rb-stale",
	})
	var stale *StaleReloadError
	if !errors.As(err, &stale) {
		t.Fatalf("expected StaleReloadError, got %v", err)
	}
	t.Logf("stale rollback rejected: %v", err)

	if _, err := m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 2, TargetRevision: 1, RequestID: "rb-dup",
	}); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	_, err = m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 3, TargetRevision: 2, RequestID: "rb-dup",
	})
	var dup *DuplicateReloadError
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateReloadError, got %v", err)
	}
	t.Logf("duplicate rollback rejected: %v", err)
}

// 历史保留上限：超出淘汰最旧；边界（数量恰好等于上限）不淘汰；
// 调低上限立即生效；当前版本始终保留。
func TestHistoryRetentionBounded(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetHistoryLimit(3)

	for i := 0; i < 9; i++ {
		ov.Set(RawConfig{"server": map[string]any{"port": int64(20000 + i)}})
		if _, err := m.Reload(context.Background(), ReloadRequest{
			ExpectedRevision: int64(i + 1), RequestID: fmt.Sprintf("r-%d", i),
		}); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	hist := m.History(0)
	logHistory(t, "limit=3 after 10 revisions", hist)
	if len(hist) != 3 || hist[0].Revision != 10 || hist[2].Revision != 8 {
		t.Fatalf("expected retained revisions [10 9 8], got %+v", hist)
	}
	// 边界：限制为 2 时保留 [10 9]，rev8 立即淘汰。
	m.SetHistoryLimit(2)
	hist = m.History(0)
	if len(hist) != 2 || hist[1].Revision != 9 {
		t.Fatalf("expected retained revisions [10 9] after shrink, got %+v", hist)
	}
	// rev9 仍可回滚（在保留窗口内）。
	if _, err := m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 10, TargetRevision: 9, RequestID: "rb-9",
	}); err != nil {
		t.Fatalf("rollback to retained rev9: %v", err)
	}
	// 回滚产生 rev11，rev9 被挤出局；rev8 早已淘汰。
	if _, err := m.Rollback(context.Background(), RollbackRequest{
		ExpectedRevision: 11, TargetRevision: 8, RequestID: "rb-8",
	}); err == nil {
		t.Fatal("rollback to evicted rev8 must fail")
	}
	// 正在被读取的引用不因淘汰而读坏：旧快照内容依然完整可读。
	snap := m.Current()
	if snap.Values["server.port"] != int64(20000+7) { // rev9 的内容
		t.Fatalf("retained snapshot content wrong: %v", snap.Values["server.port"])
	}
}

// 并发预演/发布/回滚 + 持续读取：任何时刻快照内部一致，
// 历史与去重集有界，-race 下无数据竞争。
func TestConcurrentPreviewPublishRollback(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetHistoryLimit(8)

	const workers = 6
	const iters = 30
	start := make(chan struct{})
	errs := make(chan string, workers*iters)
	var wg sync.WaitGroup

	// 发布者：先预演再发布；版本竞争导致的 stale/mismatch 是合法结果。
	for w := 0; w < workers/2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				ov.Set(RawConfig{"server": map[string]any{"port": int64(20000 + id*1000 + i)}})
				p, err := m.Preview(context.Background())
				if err != nil {
					errs <- "preview: " + err.Error()
					return
				}
				if !p.Acceptable {
					errs <- fmt.Sprintf("preview unexpectedly rejected: %+v", p.Rejections)
					return
				}
				_, err = m.Publish(context.Background(), PublishRequest{
					ExpectedRevision: p.BaseRevision,
					RequestID:        fmt.Sprintf("pub-%d-%d", id, i),
					Preview:          p,
				})
				switch {
				case err == nil:
				case errors.As(err, new(*StaleReloadError)),
					errors.As(err, new(*PreviewMismatchError)):
				default:
					errs <- "publish: " + err.Error()
					return
				}
			}
		}(w)
	}

	// 回滚者：目标取当前保留窗口内最旧一版；被淘汰是合法结果。
	for w := 0; w < workers/2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				hist := m.History(0)
				if len(hist) < 2 {
					continue
				}
				target := hist[len(hist)-1].Revision
				cur := m.Current().Revision
				_, err := m.Rollback(context.Background(), RollbackRequest{
					ExpectedRevision: cur,
					TargetRevision:   target,
					RequestID:        fmt.Sprintf("rb-%d-%d", id, i),
				})
				switch {
				case err == nil:
				case errors.As(err, new(*StaleReloadError)),
					errors.As(err, new(*VersionNotFoundError)):
				default:
					errs <- "rollback: " + err.Error()
					return
				}
			}
		}(w)
	}

	// 读者：校验快照内部一致性（每个值都有来源溯源，端口在合法范围）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 3000; i++ {
			s := m.Current()
			if s == nil {
				continue
			}
			paths := make([]string, 0, len(s.Values))
			for p := range s.Values {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				if s.Sources[p] == "" {
					errs <- fmt.Sprintf("HALF-STATE: rev=%d path=%s has no source", s.Revision, p)
					return
				}
			}
			port := s.Values["server.port"].(int64)
			if port < 1 || port > 65535 {
				errs <- fmt.Sprintf("HALF-STATE: rev=%d port=%d out of range", s.Revision, port)
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	hist := m.History(0)
	logHistory(t, "after concurrent churn", hist)
	if len(hist) > 8 {
		t.Fatalf("history exceeded limit: %d", len(hist))
	}
	if m.seen.len() > maxRememberedRequestIDs {
		t.Fatalf("request id set unbounded: %d", m.seen.len())
	}
	t.Logf("concurrent churn settled at revision %d, retained history %d versions",
		m.Current().Revision, len(hist))
}

// 高频发布/回滚压力：历史与去重集占用有界，不随时间增长。
func TestHighFrequencyChurnBoundedMemory(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, validFileCfg(9090), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.SetHistoryLimit(4)

	const cycles = 2000
	for i := 0; i < cycles; i++ {
		cur := m.Current().Revision
		ov.Set(RawConfig{"server": map[string]any{"port": int64(20000 + i%1000)}})
		if _, err := m.Reload(context.Background(), ReloadRequest{
			ExpectedRevision: cur, RequestID: fmt.Sprintf("churn-%d", i),
		}); err != nil {
			t.Fatalf("cycle %d reload: %v", i, err)
		}
		// 回滚到上一版（一定在保留窗口内）。
		if _, err := m.Rollback(context.Background(), RollbackRequest{
			ExpectedRevision: cur + 1, TargetRevision: cur, RequestID: fmt.Sprintf("churn-rb-%d", i),
		}); err != nil {
			t.Fatalf("cycle %d rollback: %v", i, err)
		}
		if len(m.History(0)) > 4 {
			t.Fatalf("cycle %d: history exceeded limit", i)
		}
	}
	if got := len(m.History(0)); got != 4 {
		t.Fatalf("expected 4 retained versions, got %d", got)
	}
	if got := m.seen.len(); got > maxRememberedRequestIDs {
		t.Fatalf("request id set unbounded: %d entries", got)
	}
	t.Logf("after %d publish/rollback cycles: revision=%d, history=%d, remembered ids=%d (cap %d)",
		cycles, m.Current().Revision, len(m.History(0)), m.seen.len(), maxRememberedRequestIDs)
}
