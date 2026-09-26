package config

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 原子性：并发重载期间，读取方只能看到完整的旧快照或新快照，
// 不得观察到 port 已变而 host 未变的半更新状态。
func TestConcurrentReloadAtomicity(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.host": "h0", "server.port": "100", "db.dsn": "x",
	})
	m, err := NewManager(context.Background(), testSchema(), file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读取方：通过一致视图持续观察 host+port 的一致性
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				view := m.Snapshot()
				if view == nil {
					continue
				}
				host, _ := view.Get("server.host")
				port, _ := view.Get("server.port")
				// 成对更新：host=hN 时 port 必须是 10N 之外规则化的对应值
				var expect string
				switch host {
				case "h0":
					expect = "100"
				case "h1":
					expect = "101"
				case "h2":
					expect = "102"
				default:
					t.Errorf("reader %d observed unknown host %q", id, host)
					return
				}
				if port != expect {
					t.Errorf("reader %d observed torn state: host=%q port=%q", id, host, port)
					return
				}
			}
		}(r)
	}

	// 写入方：串行重载三轮，每轮成对变更两个键
	for i := 1; i <= 2; i++ {
		file.Set("server.host", fmt.Sprintf("h%d", i))
		file.Set("server.port", fmt.Sprintf("10%d", i))
		if _, err := m.Reload(context.Background(), 0); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if v, _ := m.Get("server.host"); v != "h2" {
		t.Fatalf("final host: want h2, got %q", v)
	}
	t.Logf("final snapshot version=%d host=h2 port=102 consistent", m.Version())
}

// 过期/重复的重载请求须被明确拒绝。
func TestStaleReloadRejected(t *testing.T) {
	m, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{"db.dsn": "x"}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v1 := m.Version()

	// 基于正确版本的重载成功
	v2, err := m.Reload(context.Background(), v1)
	if err != nil {
		t.Fatalf("Reload with current version: %v", err)
	}
	if v2 != v1+1 {
		t.Fatalf("version: want %d, got %d", v1+1, v2)
	}

	// 重复同一 expectVersion（已过期）→ 拒绝
	_, err = m.Reload(context.Background(), v1)
	if ReasonOf(err) != ReasonStaleReload {
		t.Fatalf("want stale reload rejection, got %v", err)
	}
	t.Logf("duplicate reload with expectVersion=%d rejected: reason=%s err=%v",
		v1, ReasonOf(err), err)

	// 未来版本同样拒绝
	_, err = m.Reload(context.Background(), v2+99)
	if ReasonOf(err) != ReasonStaleReload {
		t.Fatalf("want stale reload rejection for future version, got %v", err)
	}
}

// 重载失败：保留上一份可用配置，错误原因可区分。
func TestFailedReloadKeepsPrevious(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.port": "8080", "db.dsn": "good-dsn",
	})
	m, err := NewManager(context.Background(), testSchema(), file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	before := m.Version()

	// 使来源产生非法值后重载
	file.Set("server.port", "not-a-number")
	ver, err := m.Reload(context.Background(), 0)
	if err == nil {
		t.Fatal("expected reload failure")
	}
	t.Logf("reload with port=not-a-number rejected: reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonTypeMismatch {
		t.Fatalf("reason: want %s, got %s", ReasonTypeMismatch, ReasonOf(err))
	}
	if ver != before || m.Version() != before {
		t.Fatalf("version must stay %d after failed reload, got %d", before, m.Version())
	}
	// 旧配置仍可用
	if v, _ := m.Get("server.port"); v != "8080" {
		t.Fatalf("old config must survive failed reload, got port=%q", v)
	}
	if v, _ := m.Get("db.dsn"); v != "good-dsn" {
		t.Fatalf("old dsn must survive, got %q", v)
	}

	// 修复来源后重载恢复成功
	file.Set("server.port", "9090")
	if _, err := m.Reload(context.Background(), 0); err != nil {
		t.Fatalf("Reload after fix: %v", err)
	}
	if v, _ := m.Get("server.port"); v != "9090" {
		t.Fatalf("want 9090 after recovery, got %q", v)
	}
	t.Logf("recovery reload ok: port=9090 version=%d", m.Version())
}

// 并发重载 + 过期版本混合：所有重载串行化，过期者被拒绝。
func TestConcurrentReloadsSerialized(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{"db.dsn": "x"})
	m, err := NewManager(context.Background(), testSchema(), file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	base := m.Version()

	var wg sync.WaitGroup
	var mu sync.Mutex
	stale, ok := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 所有 goroutine 都基于同一旧版本号发起重载：至多一个成功
			if _, err := m.Reload(context.Background(), base); err != nil {
				if ReasonOf(err) == ReasonStaleReload {
					mu.Lock()
					stale++
					mu.Unlock()
				} else {
					t.Errorf("unexpected reload error: %v", err)
				}
			} else {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	t.Logf("concurrent reloads on same expectVersion=%d: ok=%d stale=%d", base, ok, stale)
	if ok != 1 {
		t.Fatalf("exactly one reload should win, got ok=%d", ok)
	}
	if stale != 15 {
		t.Fatalf("remaining 15 must be rejected as stale, got %d", stale)
	}
}
