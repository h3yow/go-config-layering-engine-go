package config

import (
	"context"
	"testing"
)

func evolvedSchema() *Schema {
	return NewSchema().
		MustAdd("server.addr", Field{Type: TypeString, Default: ":0"}).
		MustAdd("db.dsn", Field{Type: TypeString, Required: true}).
		MustAdd("cache.ttl", Field{Type: TypeDuration, Default: "30s"}).
		AddRule(Rename("server.hostport", "server.addr")). // 字段改名
		AddRule(Rename("db.url", "db.dsn")).               // 字段改名
		AddRule(Delete("cache.oldttl", DeleteDropWarn)).   // 删除：丢弃并提示
		AddRule(Delete("legacy.unsafe", DeleteReject))     // 删除：拒绝
}

// 历史配置按声明规则迁移，并给出弃用提示。
func TestRenameMigrationWithWarning(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.hostport": ":8080",  // 旧名
		"db.url":          "pg://x", // 旧名
		"cache.oldttl":    "10s",    // 已删除字段，策略 drop_warn
	})
	m, err := NewManager(context.Background(), evolvedSchema(), file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if v, src, _ := m.Explain("server.addr"); v != ":8080" {
		t.Fatalf("migrated addr: want :8080, got %q from %q", v, src)
	}
	if v, _ := m.Get("db.dsn"); v != "pg://x" {
		t.Fatalf("migrated dsn: want pg://x, got %q", v)
	}
	warns := m.Warnings()
	for _, w := range warns {
		t.Logf("deprecation warning: path=%s source=%s msg=%q", w.Path, w.Source, w.Msg)
	}
	if len(warns) != 3 {
		t.Fatalf("want 3 warnings (2 rename + 1 drop), got %d", len(warns))
	}
}

// 新旧键同时出现且值冲突：拒绝，不得静默选边。
func TestRenameConflictRejected(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.hostport": ":8080",
		"server.addr":     ":9090", // 与旧键冲突
		"db.dsn":          "x",
	})
	_, err := NewManager(context.Background(), evolvedSchema(), file)
	if err == nil {
		t.Fatal("expected migration conflict rejection")
	}
	t.Logf("conflicting old/new keys -> rejected reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonMigrationFailure {
		t.Fatalf("reason: want %s, got %s", ReasonMigrationFailure, ReasonOf(err))
	}
}

// 新旧键同时出现但值相同：接受并提示（幂等迁移）。
func TestRenameSameValueAccepted(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.hostport": ":8080",
		"server.addr":     ":8080",
		"db.dsn":          "x",
	})
	m, err := NewManager(context.Background(), evolvedSchema(), file)
	if err != nil {
		t.Fatalf("same-value rename should be accepted: %v", err)
	}
	if v, _ := m.Get("server.addr"); v != ":8080" {
		t.Fatalf("want :8080, got %q", v)
	}
}

// 无法安全迁移的历史配置（reject 策略）：拒绝而非静默丢字段。
func TestDeleteRejectPolicy(t *testing.T) {
	file := NewMapSource("file", PriorityFile, map[string]string{
		"db.dsn":        "x",
		"legacy.unsafe": "1", // 已删除且策略为 reject
	})
	_, err := NewManager(context.Background(), evolvedSchema(), file)
	if err == nil {
		t.Fatal("expected rejection for unmigratable legacy key")
	}
	t.Logf("legacy.unsafe present -> rejected reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonMigrationFailure {
		t.Fatalf("reason: want %s, got %s", ReasonMigrationFailure, ReasonOf(err))
	}
}

// 可复现性：规则不变时，同一历史输入的解析结论（值/来源/告警）完全一致。
func TestMigrationReproducible(t *testing.T) {
	input := map[string]string{
		"server.hostport": ":8080",
		"db.url":          "pg://x",
		"cache.oldttl":    "10s",
	}
	type result struct {
		addr, dsn string
		warns     int
	}
	var first *result
	for i := 0; i < 20; i++ {
		m, err := NewManager(context.Background(), evolvedSchema(),
			NewMapSource("file", PriorityFile, input))
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		addr, _ := m.Get("server.addr")
		dsn, _ := m.Get("db.dsn")
		r := result{addr: addr, dsn: dsn, warns: len(m.Warnings())}
		if first == nil {
			first = &r
			t.Logf("run 0: addr=%q dsn=%q warns=%d", addr, dsn, r.warns)
			continue
		}
		if r != *first {
			t.Fatalf("run %d differs: %+v vs %+v", i, r, *first)
		}
	}
}

// 迁移后的值仍须通过目标字段的约束校验。
func TestMigratedValueStillValidated(t *testing.T) {
	schema := NewSchema().
		MustAdd("server.port", Field{Type: TypeInt, Min: "1", Max: "65535"}).
		MustAdd("db.dsn", Field{Type: TypeString, Required: true}).
		AddRule(Rename("server.portnum", "server.port"))
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.portnum": "abc", // 迁移后类型不匹配
		"db.dsn":         "x",
	})
	_, err := NewManager(context.Background(), schema, file)
	if err == nil {
		t.Fatal("expected type mismatch after migration")
	}
	t.Logf("migrated invalid value -> rejected reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonTypeMismatch {
		t.Fatalf("reason: want %s, got %s", ReasonTypeMismatch, ReasonOf(err))
	}
}
