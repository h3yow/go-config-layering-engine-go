package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 环境变量按 schema 的 Env 绑定映射到点分路径，完成类型转换与列表切分。
func TestEnvSourceMapping(t *testing.T) {
	schema := testSchema(t)
	env := map[string]string{
		"APP_NAME":      "from-env",
		"APP_PORT":      "7000",
		"APP_TLS":       "true",
		"APP_LOG_LEVEL": "warn",
		"APP_HOST":      "10.0.0.5",
	}
	src := NewEnvSource("env", schema, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	raw, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("env load: %v", err)
	}
	t.Logf("env input %v -> effective raw tree %s", env, mustJSON(raw))
	want := RawConfig{
		"app":    map[string]any{"name": "from-env"},
		"server": map[string]any{"host": "10.0.0.5", "port": int64(7000), "tls": true},
		"log":    map[string]any{"level": "warn"},
	}
	if mustJSON(raw) != mustJSON(want) {
		t.Fatalf("env tree = %s, want %s", mustJSON(raw), mustJSON(want))
	}
}

// 环境变量给出无法转换的值，必须以 KindSourceLoad 拒绝而非字符串原样注入。
func TestEnvSourceInvalidValueRejected(t *testing.T) {
	schema := testSchema(t)
	src := NewEnvSource("env", schema, func(k string) (string, bool) {
		if k == "APP_PORT" {
			return "seven-thousand", true
		}
		return "", false
	})
	_, err := src.Load(context.Background())
	var re *ResolutionError
	if !errors.As(err, &re) || re.Kind != KindSourceLoad || re.Path != "server.port" {
		t.Fatalf("want KindSourceLoad on server.port, got %v", err)
	}
	t.Logf("bad env rejected as expected: %v", err)
}

// JSON 文件来源可加载，并在格式错误时给出 KindSourceLoad。
func TestFileSourceLoad(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config.json")
	content := `{"app":{"name":"from-file"},"server":{"port":6543},"features":["a","b"]}`
	if err := os.WriteFile(good, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := NewFileSource("file", good).Load(context.Background())
	if err != nil {
		t.Fatalf("file load: %v", err)
	}
	if raw["app"].(map[string]any)["name"] != "from-file" {
		t.Fatalf("unexpected raw: %s", mustJSON(raw))
	}
	t.Logf("file input %s -> tree %s", content, mustJSON(raw))

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = NewFileSource("file", bad).Load(context.Background())
	var re *ResolutionError
	if !errors.As(err, &re) || re.Kind != KindSourceLoad {
		t.Fatalf("want KindSourceLoad for broken JSON, got %v", err)
	}
}

// 文件来源（历史 v1）经过迁移后与默认值合并，端到端验证。
func TestEndToEndFileEnvOverrideWithMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v1.json")
	// v1 风格：顶层 host/port、logging.level、debug。
	if err := os.WriteFile(path, []byte(`{"app":{"name":"e2e"},"host":"0.0.0.0","port":80,"logging":{"level":"debug"},"debug":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(testSchema(t), testRules())
	if err != nil {
		t.Fatal(err)
	}
	env := NewEnvSource("env", testSchema(t), func(k string) (string, bool) {
		if k == "APP_TLS" {
			return "true", true
		}
		return "", false
	})
	ov := NewOverrideSource("override", RawConfig{"log": map[string]any{"level": "error"}})
	m := NewManager(eng,
		NewStaticSource("defaults", RawConfig{
			"server": map[string]any{"host": "0.0.0.0", "port": int64(8080), "tls": false},
			"log":    map[string]any{"level": "info"},
		}),
		NewFileSource("file", path), env, ov)
	snap, err := m.LoadInitial(context.Background())
	if err != nil {
		t.Fatalf("e2e load: %v", err)
	}
	t.Logf("e2e sources: %s\nwarnings: %s", mustJSON(snap.Sources), mustJSON(snap.Warnings))
	if snap.Sources["server.port"] != "file" || snap.Values["server.port"] != int64(80) {
		t.Errorf("file migration should win server.port: %v", snap.Values["server.port"])
	}
	if snap.Sources["server.tls"] != "env" || snap.Values["server.tls"] != true {
		t.Errorf("env should win server.tls")
	}
	if snap.Sources["log.level"] != "override" || snap.Values["log.level"] != "error" {
		t.Errorf("override should win log.level: %v", snap.Values["log.level"])
	}
	if len(snap.Warnings) != 4 {
		t.Errorf("want 4 migration warnings, got %d", len(snap.Warnings))
	}
}
