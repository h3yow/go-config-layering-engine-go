package config

import (
	"testing"
)

func resolveForTest(t *testing.T, schema *Schema, rules []Rule, layers ...RawLayer) (*Resolved, error) {
	t.Helper()
	eng, err := NewEngine(schema, rules)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng.Resolve(layers)
}

// 默认值、文件、环境变量、运行期覆盖四层并存时，标量按来源优先级确定，对象递归合并。
func TestResolveSourcePriorityConflict(t *testing.T) {
	schema := testSchema(t)
	defs := RawLayer{Name: "defaults", Data: RawConfig{
		"app":    map[string]any{"name": "demo"},
		"server": map[string]any{"host": "0.0.0.0", "port": int64(8080), "tls": false},
		"log":    map[string]any{"level": "info"},
	}}
	file := RawLayer{Name: "file", Data: RawConfig{
		"server": map[string]any{"host": "127.0.0.1", "port": int64(9090)},
	}}
	env := RawLayer{Name: "env", Data: RawConfig{
		"server": map[string]any{"port": int64(10000)},
	}}
	override := RawLayer{Name: "override", Data: RawConfig{
		"server": map[string]any{"tls": true},
		"log":    map[string]any{"level": "debug"},
	}}

	res, err := resolveForTest(t, schema, nil, defs, file, env, override)
	logCase(t, "source priority: defaults < file < env < override",
		[]RawLayer{defs, file, env, override}, res, err)
	if err != nil {
		t.Fatalf("expected successful merge, got %v", err)
	}
	cases := map[string]struct {
		want any
		src  string
	}{
		"app.name":    {"demo", "defaults"},
		"server.host": {"127.0.0.1", "file"},
		"server.port": {int64(10000), "env"},
		"server.tls":  {true, "override"},
		"log.level":   {"debug", "override"},
	}
	for path, c := range cases {
		if got := res.Values[path]; got != c.want {
			t.Errorf("%s value = %v, want %v", path, got, c.want)
		}
		if got := res.Sources[path]; got != c.src {
			t.Errorf("%s source = %q, want %q", path, got, c.src)
		}
	}
}

// 非法端口不得静默回退默认值，必须以 KindInvalidValue 拒绝。
func TestResolveInvalidValueNoSilentDefault(t *testing.T) {
	schema := testSchema(t)
	defs := RawLayer{Name: "defaults", Data: RawConfig{
		"app":    map[string]any{"name": "demo"},
		"server": map[string]any{"port": int64(8080)},
	}}
	file := RawLayer{Name: "file", Data: RawConfig{
		"server": map[string]any{"port": int64(70000)},
	}}
	_, err := resolveForTest(t, schema, nil, defs, file)
	logCase(t, "invalid value must reject rather than fall back to default", []RawLayer{defs, file}, nil, err)
	re, ok := AsResolutionError(err)
	if !ok {
		t.Fatalf("expected ResolutionError, got %T %v", err, err)
	}
	if re.Kind != KindInvalidValue || re.Path != "server.port" || re.Source != "file" {
		t.Fatalf("unexpected error: %+v", re)
	}
}

// 类型不匹配（环境层给字符串端口）与枚举非法必须可区分。
func TestResolveTypeMismatchAndIllegalEnumAreDistinct(t *testing.T) {
	schema := testSchema(t)
	base := []RawLayer{{Name: "defaults", Data: RawConfig{
		"app":    map[string]any{"name": "demo"},
		"server": map[string]any{"port": int64(8080)},
		"log":    map[string]any{"level": "info"},
	}}}

	badType := append(append([]RawLayer{}, base...), RawLayer{Name: "env", Data: RawConfig{
		"server": map[string]any{"port": "not-a-number"},
	}})
	_, err := resolveForTest(t, schema, nil, badType...)
	logCase(t, "type mismatch from env", badType, nil, err)
	re, ok := AsResolutionError(err)
	if !ok || re.Kind != KindTypeMismatch {
		t.Fatalf("want KindTypeMismatch, got %v", err)
	}

	badEnum := append(append([]RawLayer{}, base...), RawLayer{Name: "file", Data: RawConfig{
		"log": map[string]any{"level": "verbose"},
	}})
	_, err = resolveForTest(t, schema, nil, badEnum...)
	logCase(t, "illegal enum value from file", badEnum, nil, err)
	re, ok = AsResolutionError(err)
	if !ok || re.Kind != KindInvalidValue || re.Path != "log.level" {
		t.Fatalf("want KindInvalidValue on log.level, got %v", err)
	}
}

// 必填字段全部来源缺失时必须以 KindMissingRequired 拒绝。
func TestResolveMissingRequired(t *testing.T) {
	schema := testSchema(t)
	layers := []RawLayer{{Name: "file", Data: RawConfig{
		"server": map[string]any{"port": int64(9090)},
	}}}
	_, err := resolveForTest(t, schema, nil, layers...)
	logCase(t, "missing required app.name", layers, nil, err)
	re, ok := AsResolutionError(err)
	if !ok || re.Kind != KindMissingRequired || re.Path != "app.name" {
		t.Fatalf("want KindMissingRequired on app.name, got %v", err)
	}
}

// 对象/叶子在同一路径冲突必须明确拒绝，而不是任一方被静默覆盖。
func TestResolveObjectScalarConflict(t *testing.T) {
	schema := testSchema(t)
	layers := []RawLayer{
		{Name: "defaults", Data: RawConfig{"app": map[string]any{"name": "demo"}}},
		{Name: "file", Data: RawConfig{"app": "split-brain"}},
	}
	_, err := resolveForTest(t, schema, nil, layers...)
	logCase(t, "object vs scalar conflict", layers, nil, err)
	re, ok := AsResolutionError(err)
	if !ok || re.Kind != KindTypeMismatch {
		t.Fatalf("want KindTypeMismatch, got %v", err)
	}
}
