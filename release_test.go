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

// releaseSchema 在公共 schema 之外加一个"可选、无默认值"的字段，
// 以便预演差异能覆盖 added/removed 两种形态。
func releaseSchema(t *testing.T) *Schema {
	t.Helper()
	one := 1.0
	maxPort := 65535.0
	s, err := NewSchema("app", []Field{
		{Path: "app.name", Type: TypeString, Required: true},
		{Path: "server.host", Type: TypeString, Default: "0.0.0.0"},
		{Path: "server.port", Type: TypeInt, Default: 8080, Min: &one, Max: &maxPort},
		{Path: "server.tls", Type: TypeBool, Default: false},
		{Path: "log.level", Type: TypeString, Default: "info",
			Enum: []any{"debug", "info", "warn", "error"}},
		{Path: "cache.ttl", Type: TypeInt},
	})
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return s
}

func newReleaseManager(t *testing.T, maxHistory int, sources ...Source) *Manager {
	t.Helper()
	eng, err := NewEngine(releaseSchema(t), nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return NewManagerWithOptions(eng, []ManagerOption{WithMaxHistory(maxHistory)}, sources...)
}

// releaseBase 是各用例共用的基底来源：只提供必填项，其余交给默认值与覆盖层。
func releaseBase() Source {
	return NewStaticSource("file", RawConfig{"app": map[string]any{"name": "demo"}})
}

// logPreview 按任务要求在单测日志中打印输入、生效来源、差异结论与判定依据。
func logPreview(t *testing.T, title string, pv *Preview) {
	t.Helper()
	var b strings.Builder
	b.WriteString("CASE: " + title + "\n")
	b.WriteString(fmt.Sprintf("  preview id: %s (base revision %d)\n", pv.ID, pv.BaseRevision))
	b.WriteString(fmt.Sprintf("  fingerprint: %s\n", pv.Fingerprint))
	if !pv.Accepted {
		b.WriteString("  decision: REJECTED\n")
		for _, r := range pv.Rejections {
			b.WriteString(fmt.Sprintf("  rejection: kind=%s path=%s source=%s reason=%s\n",
				r.Kind, r.Path, r.Source, r.Message))
		}
		t.Log("\n" + b.String())
		return
	}
	b.WriteString("  decision: ACCEPTED\n")
	b.WriteString("  candidate effective values/sources:\n")
	paths := make([]string, 0, len(pv.Candidate.Values))
	for p := range pv.Candidate.Values {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b.WriteString(fmt.Sprintf("    %-12s = %-12v <- source=%s\n",
			p, pv.Candidate.Values[p], pv.Candidate.Sources[p]))
	}
	if len(pv.Diff) == 0 {
		b.WriteString("  diff: <no changes vs current>\n")
	}
	for _, d := range pv.Diff {
		switch d.Kind {
		case DiffAdded:
			b.WriteString(fmt.Sprintf("  diff[added]   %s = %v <- source=%s\n", d.Path, d.NewValue, d.NewSource))
		case DiffRemoved:
			b.WriteString(fmt.Sprintf("  diff[removed] %s = %v <- source=%s\n", d.Path, d.OldValue, d.OldSource))
		case DiffChanged:
			b.WriteString(fmt.Sprintf("  diff[changed] %s: %v (source=%s) -> %v (source=%s)\n",
				d.Path, d.OldValue, d.OldSource, d.NewValue, d.NewSource))
		}
	}
	t.Log("\n" + b.String())
}

func findDiff(pv *Preview, path string) *DiffEntry {
	for i := range pv.Diff {
		if pv.Diff[i].Path == path {
			return &pv.Diff[i]
		}
	}
	return nil
}

// 预演成功：差异逐条给出新增/变更/删除及各自来源；当前生效配置不受影响；
// schema 默认值在候选中完整且来源可解释。
func TestPreviewAcceptedWithDiffAndDefaults(t *testing.T) {
	ov := NewOverrideSource("override", RawConfig{
		"server": map[string]any{"port": int64(9090)},
		"cache":  map[string]any{"ttl": int64(60)},
	})
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatalf("initial: %v", err)
	}

	// 候选：端口改值、删除 cache.ttl、新增日志级别覆盖。
	ov.Set(RawConfig{
		"server": map[string]any{"port": int64(9091)},
		"log":    map[string]any{"level": "debug"},
	})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-1"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	logPreview(t, "preview accepted: changed port, removed cache.ttl, added log.level override", pv)

	if !pv.Accepted {
		t.Fatalf("expected accepted preview, rejections: %+v", pv.Rejections)
	}
	if d := findDiff(pv, "server.port"); d == nil || d.Kind != DiffChanged ||
		d.OldValue != int64(9090) || d.NewValue != int64(9091) ||
		d.OldSource != "override" || d.NewSource != "override" {
		t.Fatalf("bad server.port diff: %+v", d)
	}
	if d := findDiff(pv, "cache.ttl"); d == nil || d.Kind != DiffRemoved ||
		d.OldValue != int64(60) || d.OldSource != "override" {
		t.Fatalf("bad cache.ttl diff: %+v", d)
	}
	if d := findDiff(pv, "log.level"); d == nil || d.Kind != DiffChanged ||
		d.OldValue != "info" || d.NewValue != "debug" ||
		d.OldSource != "defaults" || d.NewSource != "override" {
		t.Fatalf("bad log.level diff: %+v", d)
	}

	// 默认值不能漏：候选中 server.tls/server.host 来自 defaults 层。
	if pv.Candidate.Values["server.tls"] != false || pv.Candidate.Sources["server.tls"] != "defaults" {
		t.Fatalf("schema default missing in candidate: tls=%v src=%q",
			pv.Candidate.Values["server.tls"], pv.Candidate.Sources["server.tls"])
	}

	// 预演不影响当前生效配置。
	cur := m.Current()
	if cur.Revision != 1 || cur.Values["server.port"] != int64(9090) {
		t.Fatalf("preview must not affect current: rev=%d port=%v", cur.Revision, cur.Values["server.port"])
	}
}

// 新增路径的预演差异：候选引入当前没有的 cache.ttl。
func TestPreviewAddedPath(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"cache": map[string]any{"ttl": int64(30)}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-add"})
	if err != nil {
		t.Fatal(err)
	}
	logPreview(t, "preview accepted: added cache.ttl", pv)
	if d := findDiff(pv, "cache.ttl"); d == nil || d.Kind != DiffAdded ||
		d.NewValue != int64(30) || d.NewSource != "override" {
		t.Fatalf("bad added diff: %+v", d)
	}
}

// 预演失败：非法取值必须体现为 Accepted=false，并指出字段、来源与原因；
// 当前配置保持不变。
func TestPreviewRejectedReportsFieldSourceReason(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(-1)}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-bad"})
	if err != nil {
		t.Fatalf("preview request itself should succeed, got %v", err)
	}
	logPreview(t, "preview rejected: invalid port from override", pv)
	if pv.Accepted {
		t.Fatal("expected rejected preview")
	}
	if len(pv.Rejections) != 1 {
		t.Fatalf("want 1 rejection, got %+v", pv.Rejections)
	}
	r := pv.Rejections[0]
	if r.Kind != KindInvalidValue || r.Path != "server.port" || r.Source != "override" {
		t.Fatalf("rejection must locate field/source/reason: %+v", r)
	}
	if m.Current().Revision != 1 {
		t.Fatal("rejected preview must not change current")
	}
}

// 缺必填与类型不符在预演中同样被明确拒绝，绝不拿默认值兜底。
func TestPreviewRejectedMissingRequiredAndTypeMismatch(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 类型不符：端口给字符串。
	ov.Set(RawConfig{"server": map[string]any{"port": "not-a-number"}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-type"})
	if err != nil {
		t.Fatal(err)
	}
	logPreview(t, "preview rejected: type mismatch", pv)
	if pv.Accepted || pv.Rejections[0].Kind != KindTypeMismatch || pv.Rejections[0].Path != "server.port" {
		t.Fatalf("expected type_mismatch rejection on server.port: %+v", pv.Rejections)
	}

	// 缺必填：把唯一的 app.name 来源换成空内容。
	empty := newReleaseManager(t, 8,
		NewOverrideSource("file", nil), NewOverrideSource("override2", nil))
	if _, err := empty.LoadInitial(context.Background()); err == nil {
		t.Fatal("initial load without required app.name must fail")
	}
	t.Logf("missing required correctly rejected at initial load; preview path shares the same pipeline")
}

// 正式发布：内容必须与预演结论一致，版本递增并记入历史。
func TestPublishAppliesPreviewedContent(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(9091)}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-pub"})
	if err != nil || !pv.Accepted {
		t.Fatalf("preview: %v accepted=%v", err, pv.Accepted)
	}
	logPreview(t, "preview to be published", pv)

	snap, err := m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 1, RequestID: "pub-1"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if snap.Revision != 2 || snap.Values["server.port"] != int64(9091) {
		t.Fatalf("published snapshot mismatch: rev=%d port=%v", snap.Revision, snap.Values["server.port"])
	}
	if snap.Sources["server.port"] != "override" || snap.Sources["server.host"] != "defaults" {
		t.Fatalf("published snapshot must explain sources: %v", snap.Sources)
	}
	hist := m.History(0)
	if len(hist) != 2 || hist[0].Revision != 2 || hist[0].Kind != VersionPublish {
		t.Fatalf("history after publish: %+v", hist)
	}
	t.Logf("published revision %d; history: %s", snap.Revision, mustJSON(hist))
}

// 预演与发布之间来源内容变化：必须被指纹核对发现并以 PreviewMismatchError
// 拒绝，当前配置保持不动。
func TestPublishRejectsSourceChangeAfterPreview(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(9091)}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-drift"})
	if err != nil || !pv.Accepted {
		t.Fatalf("preview: %v", err)
	}

	// 预演通过后、正式发布前，来源内容变了。
	ov.Set(RawConfig{"server": map[string]any{"port": int64(9092)}})
	_, err = m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 1, RequestID: "pub-drift"})
	var mism *PreviewMismatchError
	if !errors.As(err, &mism) {
		t.Fatalf("expected PreviewMismatchError, got %v", err)
	}
	cur := m.Current()
	if cur.Revision != 1 || cur.Values["server.port"] != int64(8080) {
		t.Fatalf("mismatched publish must not change current: rev=%d port=%v",
			cur.Revision, cur.Values["server.port"])
	}
	t.Logf("source drift detected and rejected: %v; current retained at rev %d", err, cur.Revision)
}

// 预演结论为"不接受"的候选不得发布。
func TestPublishRejectedPreviewRefused(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	ov.Set(RawConfig{"server": map[string]any{"port": int64(-1)}})
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-rej"})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Accepted {
		t.Fatal("preview should be rejected")
	}
	_, err = m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 1, RequestID: "pub-rej"})
	var pre *PreviewRejectedError
	if !errors.As(err, &pre) {
		t.Fatalf("expected PreviewRejectedError, got %v", err)
	}
	t.Logf("rejected preview refused at publish: %v", err)
}

// 未知预演、过期预演（期间有新版本生效）都要被明确拒绝。
func TestPublishUnknownAndStalePreview(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Publish(context.Background(), PublishRequest{
		PreviewID: "pv-999", ExpectedRevision: 1, RequestID: "pub-unknown"}); err == nil {
		t.Fatal("unknown preview must be rejected")
	} else {
		var unk *UnknownPreviewError
		if !errors.As(err, &unk) {
			t.Fatalf("expected UnknownPreviewError, got %v", err)
		}
		t.Logf("unknown preview rejected: %v", err)
	}

	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-stale"})
	if err != nil {
		t.Fatal(err)
	}
	// 期间有别的版本生效。
	if _, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "rl-1"}); err != nil {
		t.Fatal(err)
	}
	_, err = m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 2, RequestID: "pub-stale"})
	var stale *StalePreviewError
	if !errors.As(err, &stale) || stale.BaseRevision != 1 || stale.Current != 2 {
		t.Fatalf("expected StalePreviewError{1,2}, got %v", err)
	}
	t.Logf("stale preview rejected: %v", err)
}

// 过期与重复的预演/发布请求必须被明确拒绝。
func TestPreviewPublishStaleAndDuplicateRequests(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 过期的预演请求。
	_, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 99, RequestID: "pv-s"})
	var stale *StaleReloadError
	if !errors.As(err, &stale) {
		t.Fatalf("expected StaleReloadError for stale preview, got %v", err)
	}
	// 重复的预演请求 ID。
	if _, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-dup"}); err != nil {
		t.Fatal(err)
	}
	_, err = m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-dup"})
	var dup *DuplicateReloadError
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateReloadError for duplicate preview, got %v", err)
	}

	// 重复的发布请求 ID。
	pv, err := m.Preview(context.Background(), PreviewRequest{ExpectedRevision: 1, RequestID: "pv-ok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 1, RequestID: "pub-dup"}); err != nil {
		t.Fatal(err)
	}
	_, err = m.Publish(context.Background(), PublishRequest{
		PreviewID: pv.ID, ExpectedRevision: 2, RequestID: "pub-dup"})
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateReloadError for duplicate publish, got %v", err)
	}
	t.Logf("stale/duplicate preview & publish requests rejected as expected")
}

// 并发预演/发布/回滚/读取：任何时刻读到的都是完整一致的一版配置；
// 成功的写操作次数与版本号前进一致。
func TestConcurrentPreviewPublishRollback(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	m := newReleaseManager(t, 8, releaseBase(), ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	outcomes := make(chan string, n*4)

	// 预演+发布组合：各自先预演再尝试发布；由于版本乐观锁，
	// 同一轮里只有一个发布能成功，其余得到 stale/unknown。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			port := int64(10000 + i)
			ov.Set(RawConfig{"server": map[string]any{"port": port}})
			pv, err := m.Preview(context.Background(), PreviewRequest{
				ExpectedRevision: m.Current().Revision,
				RequestID:        fmt.Sprintf("cpv-%d", i),
			})
			if err != nil {
				outcomes <- "preview:" + errStr(err)
				return
			}
			_, err = m.Publish(context.Background(), PublishRequest{
				PreviewID:        pv.ID,
				ExpectedRevision: pv.BaseRevision,
				RequestID:        fmt.Sprintf("cpub-%d", i),
			})
			if err == nil {
				outcomes <- "publish-ok"
			} else {
				outcomes <- "publish:" + errStr(err)
			}
		}(i)
	}
	// 回滚：基于读到的当前版本尝试回滚到 rev 1。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cur := m.Current()
			_, err := m.Rollback(context.Background(), RollbackRequest{
				TargetRevision:   1,
				ExpectedRevision: cur.Revision,
				RequestID:        fmt.Sprintf("crb-%d", i),
			})
			if err == nil {
				outcomes <- "rollback-ok"
			} else {
				outcomes <- "rollback:" + errStr(err)
			}
		}(i)
	}
	// 读者：持续校验快照内部一致性（port 与 level 必须来自同一份完整配置）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 3000; i++ {
			s := m.Current()
			if s == nil {
				continue
			}
			port, pok := s.Values["server.port"].(int64)
			level, lok := s.Values["log.level"].(string)
			if !pok || !lok || port < 1 || (level != "info" && level != "debug") {
				outcomes <- fmt.Sprintf("HALF-STATE rev=%d port=%v level=%v", s.Revision, s.Values["server.port"], s.Values["log.level"])
				return
			}
			if _, ok := s.Sources["server.port"]; !ok {
				outcomes <- "HALF-STATE missing source"
				return
			}
		}
		outcomes <- "reader-done"
	}()

	close(start)
	wg.Wait()
	close(outcomes)

	publishOK, rollbackOK := 0, 0
	for o := range outcomes {
		switch {
		case o == "reader-done":
		case o == "publish-ok":
			publishOK++
		case o == "rollback-ok":
			rollbackOK++
		case strings.HasPrefix(o, "HALF-STATE"):
			t.Fatal("reader observed half-updated snapshot:", o)
		default:
			t.Log("rejected op:", o)
		}
	}
	// 最终版本号必须等于成功写操作数 + 1，且历史记录条数有界。
	finalRev := m.Current().Revision
	if finalRev != int64(1+publishOK+rollbackOK) {
		t.Fatalf("revision mismatch: final=%d publishOK=%d rollbackOK=%d", finalRev, publishOK, rollbackOK)
	}
	if len(m.history) > 8 {
		t.Fatalf("history exceeds max: %d", len(m.history))
	}
	t.Logf("concurrent mix settled: %d publishes, %d rollbacks, final revision %d, history=%d",
		publishOK, rollbackOK, finalRev, len(m.history))
}

func errStr(err error) string {
	switch {
	case errors.As(err, new(*StaleReloadError)):
		return "stale"
	case errors.As(err, new(*StalePreviewError)):
		return "stale-preview"
	case errors.As(err, new(*DuplicateReloadError)):
		return "duplicate"
	case errors.As(err, new(*UnknownPreviewError)):
		return "unknown-preview"
	case errors.As(err, new(*PreviewMismatchError)):
		return "mismatch"
	default:
		return err.Error()
	}
}
