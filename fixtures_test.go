package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// testSchema 是各用例共用的应用配置 schema。
func testSchema(t *testing.T) *Schema {
	t.Helper()
	one := 1.0
	maxPort := 65535.0
	s, err := NewSchema("app", []Field{
		{Path: "app.name", Type: TypeString, Required: true, Env: "APP_NAME"},
		{Path: "server.host", Type: TypeString, Default: "0.0.0.0", Env: "APP_HOST"},
		{Path: "server.port", Type: TypeInt, Default: 8080, Env: "APP_PORT", Min: &one, Max: &maxPort},
		{Path: "server.tls", Type: TypeBool, Default: false, Env: "APP_TLS"},
		{Path: "log.level", Type: TypeString, Default: "info", Env: "APP_LOG_LEVEL",
			Enum: []any{"debug", "info", "warn", "error"}},
		{Path: "features", Type: TypeList, ElemType: TypeString, Default: []any{}},
	})
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return s
}

// testRules 是与历史配置配套的确定性迁移规则。
func testRules() []Rule {
	return []Rule{
		{Kind: RuleRename, From: "host", To: "server.host", Since: "v2", Reason: "server settings grouped"},
		{Kind: RuleRename, From: "port", To: "server.port", Since: "v2"},
		{Kind: RuleMove, From: "logging.level", To: "log.level", Since: "v2"},
		{Kind: RuleDrop, From: "debug", Since: "v2", Reason: "use log.level=debug instead"},
	}
}

// logCase 按任务要求在单测日志中打印输入、生效来源与判定依据。
func logCase(t *testing.T, title string, layers []RawLayer, resolved *Resolved, err error) {
	t.Helper()
	var b strings.Builder
	b.WriteString("CASE: " + title + "\n")
	for _, l := range layers {
		b.WriteString(fmt.Sprintf("  input[source=%s]: %s\n", l.Name, mustJSON(l.Data)))
	}
	if err != nil {
		b.WriteString("  decision: REJECTED\n")
		if re, ok := AsResolutionError(err); ok {
			b.WriteString(fmt.Sprintf("  reason kind: %s\n  reason path: %s\n  reason source: %s\n  detail: %s\n",
				re.Kind, re.Path, re.Source, re.Message))
		} else {
			b.WriteString("  detail: " + err.Error() + "\n")
		}
	} else {
		b.WriteString("  decision: ACCEPTED\n")
		b.WriteString("  effective values/sources (basis = higher-priority layer wins scalar; objects merge):\n")
		paths := make([]string, 0, len(resolved.Values))
		for p := range resolved.Values {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			b.WriteString(fmt.Sprintf("    %-14s = %-12v <- source=%s\n", p, resolved.Values[p], resolved.Sources[p]))
		}
		for _, w := range resolved.Warnings {
			b.WriteString(fmt.Sprintf("  warning[%s] %s: %s\n", w.Code, w.Path, w.Message))
		}
	}
	t.Log("\n" + b.String())
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<%v>", v)
	}
	return string(b)
}
