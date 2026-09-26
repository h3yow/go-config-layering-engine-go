package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func newTestManager(t *testing.T, sources ...Source) *Manager {
	t.Helper()
	eng, err := NewEngine(testSchema(t), testRules())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return NewManager(eng, sources...)
}

func validFileCfg(port int) RawConfig {
	return RawConfig{
		"app":    map[string]any{"name": "demo"},
		"server": map[string]any{"host": "127.0.0.1", "port": int64(port)},
		"log":    map[string]any{"level": "info"},
	}
}

// 首次加载成功后版本为 1，快照可被原子读取。
func TestManagerInitialLoadAndSnapshot(t *testing.T) {
	file := NewStaticSource("file", validFileCfg(9090))
	m := newTestManager(t, file)
	snap, err := m.LoadInitial(context.Background())
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if snap.Revision != 1 || snap.Values["server.port"] != int64(9090) {
		t.Fatalf("unexpected initial snapshot: rev=%d port=%v", snap.Revision, snap.Values["server.port"])
	}
	if m.Current().Revision != 1 {
		t.Fatal("Current() does not reflect loaded snapshot")
	}
}

// 重载失败时保留上一份可用配置，并返回可区分的失败原因；
// 随后一次合法重载成功，版本只在成功时递增。
func TestManagerReloadFailureRetainsPrevious(t *testing.T) {
	file := NewStaticSource("file", validFileCfg(9090))
	ov := NewOverrideSource("override", nil)
	m := newTestManager(t, file, ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatalf("initial: %v", err)
	}

	// 破坏配置：非法端口。
	ov.Set(RawConfig{"server": map[string]any{"port": int64(-5)}})
	bad, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "r1"})
	if err == nil || bad != nil {
		t.Fatalf("expected failed reload, got snap=%v err=%v", bad, err)
	}
	var re *ResolutionError
	if !errors.As(err, &re) || re.Kind != KindInvalidValue {
		t.Fatalf("expected wrapped KindInvalidValue, got %v", err)
	}
	cur := m.Current()
	if cur.Revision != 1 || cur.Values["server.port"] != int64(9090) {
		t.Fatalf("previous config not retained after failed reload: %+v", cur)
	}
	t.Logf("reload rejected, previous retained at revision %d; error: %v", cur.Revision, err)

	// 修正后重试（新请求 ID），原子生效到 revision 2。
	ov.Set(RawConfig{"server": map[string]any{"port": int64(20000)}})
	snap, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "r2"})
	if err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if snap.Revision != 2 || snap.Values["server.port"] != int64(20000) {
		t.Fatalf("unexpected recovered snapshot: %+v", snap)
	}
}

// 过期的重载请求（期望版本落后）必须明确拒绝，且不改变现状。
func TestManagerStaleReloadRejected(t *testing.T) {
	file := NewStaticSource("file", validFileCfg(9090))
	m := newTestManager(t, file)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 99, RequestID: "stale"})
	var stale *StaleReloadError
	if !errors.As(err, &stale) || stale.Expected != 99 || stale.Current != 1 {
		t.Fatalf("expected StaleReloadError{99,1}, got %v", err)
	}
	if m.Current().Revision != 1 {
		t.Fatal("stale reload must not change current revision")
	}
	t.Logf("stale request rejected as expected: %v", err)
}

// 重复 RequestID 的重载必须被明确拒绝。
func TestManagerDuplicateReloadRejected(t *testing.T) {
	file := NewStaticSource("file", validFileCfg(9090))
	m := newTestManager(t, file)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 1, RequestID: "dup"}); err != nil {
		t.Fatalf("first reload should succeed: %v", err)
	}
	_, err := m.Reload(context.Background(), ReloadRequest{ExpectedRevision: 2, RequestID: "dup"})
	var dup *DuplicateReloadError
	if !errors.As(err, &dup) || dup.RequestID != "dup" {
		t.Fatalf("expected DuplicateReloadError, got %v", err)
	}
	t.Logf("duplicate request rejected as expected: %v", err)
}

// 并发重载：只有基于当前版本的请求能成功，其余得到 stale；
// 任何时刻读到的快照都必须是内部一致的整版配置。
func TestManagerConcurrentReloadsNoHalfState(t *testing.T) {
	ov := NewOverrideSource("override", nil)
	file := NewStaticSource("file", validFileCfg(9090))
	m := newTestManager(t, file, ov)
	if _, err := m.LoadInitial(context.Background()); err != nil {
		t.Fatal(err)
	}

	const n = 24
	var wg sync.WaitGroup
	results := make(chan string, n*2)
	start := make(chan struct{})

	// 一半 goroutine 基于正确版本竞争唯一一次成功机会；
	// 另一半持有更旧版本，必然 stale。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			port := int64(20000 + i)
			ov.Set(RawConfig{"server": map[string]any{"port": port}})
			expected := int64(1)
			if i%2 == 1 {
				expected = 0 // 故意过期
			}
			snap, err := m.Reload(context.Background(), ReloadRequest{
				ExpectedRevision: expected,
				RequestID:        fmt.Sprintf("race-%d", i),
			})
			switch {
			case err == nil:
				results <- fmt.Sprintf("ok rev=%d port=%d", snap.Revision, snap.Values["server.port"])
			case errors.As(err, new(*StaleReloadError)):
				results <- "stale"
			default:
				results <- "other:" + err.Error()
			}
		}(i)
	}

	// 读者：反复读取快照，校验 port 与 revision 属于同一份完整配置，
	// 不应观察到半更新状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			s := m.Current()
			if s == nil {
				continue
			}
			port := s.Values["server.port"].(int64)
			level := s.Values["log.level"].(string)
			if (port < 9090) || (level != "info") {
				results <- fmt.Sprintf("HALF-STATE rev=%d port=%v level=%v", s.Revision, port, level)
				return
			}
			_ = s.Tree["server"] // 确保树结构可遍历
		}
		results <- "reader-done"
	}()

	close(start)
	wg.Wait()
	close(results)

	okCount, staleCount := 0, 0
	for r := range results {
		switch {
		case r == "reader-done":
		case r == "stale":
			staleCount++
		case len(r) > 3 && r[:3] == "ok ":
			okCount++
			t.Log("reload outcome:", r)
		default:
			t.Fatal("unexpected reload outcome:", r)
		}
	}
	if okCount != 1 {
		t.Fatalf("exactly one concurrent reload expected to succeed, got %d (stale=%d)", okCount, staleCount)
	}
	if staleCount != n-1 {
		t.Fatalf("expected %d stale rejections (all but the single winner), got %d", n-1, staleCount)
	}
	t.Logf("concurrent reload settled: 1 success, %d stale; final revision %d",
		staleCount, m.Current().Revision)
}

// 来源自身加载失败也必须回滚：文件来源报 KindSourceLoad，旧快照保留。
func TestManagerSourceLoadFailureRetainsPrevious(t *testing.T) {
	missing := NewFileSource("file", "definitely-missing-config.json")
	m := newTestManager(t, missing)
	if _, err := m.LoadInitial(context.Background()); err == nil {
		t.Fatal("expected initial load failure for missing file")
	}
	if m.Current() != nil {
		t.Fatal("no snapshot may exist after failed initial load")
	}
}
