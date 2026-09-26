package config

import (
	"testing"
)

// 历史配置按 rename/move/drop 规则确定性迁移，并给出弃用提示。
func TestMigrationHistoricalConfigAppliesRules(t *testing.T) {
	schema := testSchema(t)
	rules := testRules()
	layers := []RawLayer{{Name: "file:v1", Data: RawConfig{
		"app":     map[string]any{"name": "legacy"},
		"host":    "10.0.0.1",
		"port":    int64(7000),
		"debug":   true,
		"logging": map[string]any{"level": "warn"},
	}}}
	res, err := resolveForTest(t, schema, rules, layers...)
	logCase(t, "historical v1 config migrated", layers, res, err)
	if err != nil {
		t.Fatalf("migration should succeed: %v", err)
	}
	if res.Values["server.host"] != "10.0.0.1" {
		t.Errorf("host migration failed: %v", res.Values["server.host"])
	}
	if res.Values["server.port"] != int64(7000) {
		t.Errorf("port migration failed: %v", res.Values["server.port"])
	}
	if res.Values["log.level"] != "warn" {
		t.Errorf("logging.level move failed: %v", res.Values["log.level"])
	}
	if _, present := getPath(res.Tree, "debug"); present {
		t.Errorf("dropped key still present")
	}
	if len(res.Warnings) != 4 {
		t.Fatalf("want 4 deprecation warnings, got %d: %+v", len(res.Warnings), res.Warnings)
	}
	codes := map[string]int{}
	for _, w := range res.Warnings {
		codes[w.Code]++
	}
	if codes[WarnDeprecatedRename] != 2 || codes[WarnDeprecatedMove] != 1 || codes[WarnDeprecatedDrop] != 1 {
		t.Fatalf("unexpected warning distribution: %+v", codes)
	}
}

// 新旧路径同时出现属于无法安全迁移，必须拒绝，不能丢字段。
func TestMigrationConflictRejected(t *testing.T) {
	schema := testSchema(t)
	rules := testRules()
	layers := []RawLayer{{Name: "file", Data: RawConfig{
		"app":    map[string]any{"name": "legacy"},
		"host":   "10.0.0.1",
		"server": map[string]any{"host": "127.0.0.1"},
	}}}
	_, err := resolveForTest(t, schema, rules, layers...)
	logCase(t, "rename conflict: old and new path both set", layers, nil, err)
	re, ok := AsResolutionError(err)
	if !ok || re.Kind != KindMigrationConflict {
		t.Fatalf("want KindMigrationConflict, got %v", err)
	}
}

// 无规则覆盖的历史字段必须以 KindUnknownField 拒绝，而不是静默丢弃。
func TestMigrationUnknownFieldRejected(t *testing.T) {
	schema := testSchema(t)
	layers := []RawLayer{{Name: "file", Data: RawConfig{
		"app":         map[string]any{"name": "legacy"},
		"mystery_opt": true,
	}}}
	_, err := resolveForTest(t, schema, testRules(), layers...)
	logCase(t, "unknown historical field cannot be dropped silently", layers, nil, err)
	re, ok := AsResolutionError(err)
	if !ok || re.Kind != KindUnknownField || re.Path != "mystery_opt" {
		t.Fatalf("want KindUnknownField on mystery_opt, got %v", err)
	}
}

// 规则不变时同一历史输入的解析结论必须可复现（跑两遍逐字节比较）。
func TestMigrationDeterministicReproducible(t *testing.T) {
	schema := testSchema(t)
	rules := testRules()
	run := func() *Resolved {
		layers := []RawLayer{{Name: "file:v1", Data: RawConfig{
			"app":     map[string]any{"name": "legacy"},
			"host":    "10.0.0.1",
			"port":    int64(7000),
			"debug":   true,
			"logging": map[string]any{"level": "warn"},
		}}}
		res, err := resolveForTest(t, &*schema, rules, layers...)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return res
	}
	a, b := run(), run()
	if mustJSON(a.Tree) != mustJSON(b.Tree) ||
		mustJSON(a.Sources) != mustJSON(b.Sources) ||
		mustJSON(a.Warnings) != mustJSON(b.Warnings) {
		t.Fatalf("non-reproducible resolution:\nA: %s\nB: %s", mustJSON(a), mustJSON(b))
	}
}

// 自相矛盾的规则声明（同一来源两条规则）在构造引擎时即被拒绝。
func TestMigrationRuleSelfValidation(t *testing.T) {
	bad := []Rule{
		{Kind: RuleRename, From: "host", To: "server.host"},
		{Kind: RuleDrop, From: "host"},
	}
	if _, err := NewEngine(testSchema(t), bad); err == nil {
		t.Fatal("expected duplicate migration source to be rejected")
	}
}
