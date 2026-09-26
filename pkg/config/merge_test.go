package config

import (
	"context"
	"testing"
)

func testSchema() *Schema {
	return NewSchema().
		MustAdd("server.host", Field{Type: TypeString, Default: "127.0.0.1"}).
		MustAdd("server.port", Field{Type: TypeInt, Min: "1", Max: "65535", Default: "8080"}).
		MustAdd("log.level", Field{Type: TypeString, Enum: []string{"debug", "info", "warn", "green"}, Default: "info"}).
		MustAdd("db.dsn", Field{Type: TypeString, Required: true}).
		MustAdd("http.timeout", Field{Type: TypeDuration, Min: "1s", Max: "1m", Default: "5s"})
}

func logDecision(t *testing.T, input string, m *Manager, path string) {
	t.Helper()
	v, src, ok := m.Explain(path)
	t.Logf("input=%s field=%s effective=%q source=%q found=%v", input, path, v, src, ok)
}

// 来源冲突：override > env > file > defaults，结论与生效层级须可解释。
func TestSourcePriorityConflict(t *testing.T) {
	defaults := NewMapSource("defaults", PriorityDefaults, map[string]string{
		"server.port": "1000", "db.dsn": "default " + "default-dsn",
	})
	file := NewMapSource("file", PriorityFile, map[string]string{
		"server.port": "2000", "db.dsn": "file-dsn",
	})
	env := NewMapSource("env", PriorityEnv, map[string]string{
		"server.port": "3000",
	})
	override := NewMapSource("override", PriorityOverride, map[string]string{})

	m, err := NewManager(context.Background(), testSchema(), defaults, file, env, override)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	logDecision(t, "port: defaults=1000 file=2000 env=3000", m, "server.port")
	if v, _ := m.Get("server.port"); v != "3000" {
		t.Fatalf("port: want env value 3000, got %q", v)
	}
	logDecision(t, "dsn: defaults=file 均有, file 优先", m, "db.dsn")
	if v, _ := m.Get("db.dsn"); v != "file-dsn" {
		t.Fatalf("dsn: want file-dsn, got %q", v)
	}

	override.Set("server.port", "4000")
	if _, err := m.Reload(context.Background(), 0); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	logDecision(t, "port: override=4000 加入后", m, "server.port")
	if v, _ := m.Get("server.port"); v != "4000" {
		t.Fatalf("port: want override value 4000, got %q", v)
	}
}

// 非法取值：类型不匹配 / 枚举外取值 / 超范围，均不得回退默认值。
func TestInvalidValuesRejected(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		val    string
		reason Reason
	}{
		{"type_mismatch_int", "server.port", "not-a-number", ReasonTypeMismatch},
		{"enum_violation", "log.level", "verbose", ReasonInvalidValue},
		{"below_min", "server.port", "0", ReasonInvalidValue},
		{"above_max_duration", "http.timeout", "2m", ReasonInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := NewMapSource("file", PriorityFile, map[string]string{
				"db.dsn": "x", tc.key: tc.val,
			})
			_, err := NewManager(context.Background(), testSchema(), base)
			if err == nil {
				t.Fatalf("input %s=%q: expected rejection, got success", tc.key, tc.val)
			}
			got := ReasonOf(err)
			t.Logf("input %s=%q -> rejected reason=%s err=%v", tc.key, tc.val, got, err)
			if got != tc.reason {
				t.Fatalf("reason: want %s, got %s", tc.reason, got)
			}
		})
	}
}

// 必填缺失：不得静默回退。
func TestMissingRequired(t *testing.T) {
	m, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{}))
	if err == nil {
		t.Fatalf("expected missing required error, got manager %+v", m)
	}
	t.Logf("empty sources -> rejected reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonMissingRequired {
		t.Fatalf("reason: want %s, got %s", ReasonMissingRequired, ReasonOf(err))
	}
}

// 未声明字段：拒绝而非静默丢弃。
func TestUnknownFieldRejected(t *testing.T) {
	_, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{
			"db.dsn": "x", "old.removed.key": "1",
		}))
	if err == nil {
		t.Fatal("expected unknown field rejection")
	}
	t.Logf("unknown field -> rejected reason=%s err=%v", ReasonOf(err), err)
	if ReasonOf(err) != ReasonInvalidValue {
		t.Fatalf("reason: want %s, got %s", ReasonInvalidValue, ReasonOf(err))
	}
}

// 默认值生效且来源可解释。
func TestDefaultsApplied(t *testing.T) {
	m, err := NewManager(context.Background(), testSchema(),
		NewMapSource("file", PriorityFile, map[string]string{"db.dsn": "x"}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	logDecision(t, "仅提供 db.dsn", m, "server.host")
	v, src, _ := m.Explain("server.host")
	if v != "127.0.0.1" || src != "default" {
		t.Fatalf("want default 127.0.0.1 from 'default', got %q from %q", v, src)
	}
}

// 文件 + 环境变量来源端到端。
func TestFileAndEnvSources(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/conf.json"
	if err := writeFile(path, `{"server":{"port": 9000}, "db":{"dsn":"file-dsn"}, "tags":["a","b"]}`); err != nil {
		t.Fatal(err)
	}
	schema := testSchema().MustAdd("tags", Field{Type: TypeStringSlice})
	m, err := NewManager(context.Background(), schema,
		NewFileSource("file", PriorityFile, path),
		NewEnvSource("env", PriorityEnv, "APP", WithEnviron(func() []string {
			return []string{"APP_SERVER_PORT=9001", "OTHER_IGNORED=1"}
		})),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	logDecision(t, "file port=9000, env APP_SERVER_PORT=9001", m, "server.port")
	if v, _ := m.Get("server.port"); v != "9001" {
		t.Fatalf("env should win, got %q", v)
	}
	if v, _ := m.Get("tags"); v != "a,b" {
		t.Fatalf("tags: want a,b got %q", v)
	}
	if v, _ := m.Get("db.dsn"); v != "file-dsn" {
		t.Fatalf("dsn: want file-dsn got %q", v)
	}
}
