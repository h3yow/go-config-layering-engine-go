package config

import (
	"fmt"
)

// RuleKind 声明历史配置的兼容处理方式。
type RuleKind string

const (
	// RuleRename 改名：From 的值迁移到 To，并发出弃用提示。
	RuleRename RuleKind = "rename"
	// RuleMove 移动到新路径（语义同改名，区别仅体现在提示文案中）。
	RuleMove RuleKind = "move"
	// RuleDrop 声明该历史字段已被删除；出现时移除并给出弃用提示。
	RuleDrop RuleKind = "drop"
)

// Rule 是一条确定性的兼容迁移规则。
type Rule struct {
	Kind   RuleKind
	From   string
	To     string
	Since  string
	Reason string
}

// Warning 记录不拒绝解析、但须对外可见的弃用/迁移提示。
type Warning struct {
	Code    string
	Path    string
	Message string
}

const (
	WarnDeprecatedRename = "deprecated_rename"
	WarnDeprecatedMove   = "deprecated_move"
	WarnDeprecatedDrop   = "deprecated_drop"
)

// validateRules 在使用前检查规则声明自身是否自洽：
// From 不得重复；rename/move 必须给出 To；同一 To 不得有多个来源。
func validateRules(rules []Rule) error {
	seenFrom := map[string]bool{}
	seenTo := map[string]bool{}
	for i, r := range rules {
		switch r.Kind {
		case RuleRename, RuleMove:
			if r.From == "" || r.To == "" {
				return fmt.Errorf("config: migration rule #%d (%s) requires From and To", i, r.Kind)
			}
		case RuleDrop:
			if r.From == "" {
				return fmt.Errorf("config: migration rule #%d (drop) requires From", i)
			}
		default:
			return fmt.Errorf("config: migration rule #%d has unknown kind %q", i, r.Kind)
		}
		if seenFrom[r.From] {
			return fmt.Errorf("config: duplicate migration source %q", r.From)
		}
		seenFrom[r.From] = true
		if r.To != "" {
			if seenTo[r.To] {
				return fmt.Errorf("config: duplicate migration target %q", r.To)
			}
			seenTo[r.To] = true
		}
	}
	return nil
}

// applyMigrations 按声明顺序原地迁移历史配置树，返回弃用提示。
// 规则不变时，同一输入树必然得到同一结果（声明顺序即处理顺序，无 map 遍历）。
// 无法安全迁移（新旧路径同时存在）时返回 KindMigrationConflict，拒绝整个解析。
func applyMigrations(tree RawConfig, rules []Rule) ([]Warning, error) {
	var warnings []Warning
	for _, r := range rules {
		fromVal, fromExists := getPath(tree, r.From)
		if !fromExists {
			continue
		}
		switch r.Kind {
		case RuleDrop:
			deletePath(tree, r.From)
			warnings = append(warnings, Warning{
				Code:    WarnDeprecatedDrop,
				Path:    r.From,
				Message: dropMessage(r),
			})
		case RuleRename, RuleMove:
			if _, toExists := getPath(tree, r.To); toExists {
				return nil, &ResolutionError{
					Kind:    KindMigrationConflict,
					Path:    r.From,
					Message: fmt.Sprintf("deprecated path %q and its replacement %q are both set; remove one explicitly", r.From, r.To),
				}
			}
			if err := setPath(tree, r.To, fromVal); err != nil {
				return nil, &ResolutionError{
					Kind:    KindMigrationConflict,
					Path:    r.To,
					Message: fmt.Sprintf("cannot move %q to %q: %v", r.From, r.To, err),
				}
			}
			deletePath(tree, r.From)
			code := WarnDeprecatedRename
			if r.Kind == RuleMove {
				code = WarnDeprecatedMove
			}
			warnings = append(warnings, Warning{
				Code:    code,
				Path:    r.From,
				Message: renameMessage(r),
			})
		}
	}
	return warnings, nil
}

func renameMessage(r Rule) string {
	action := "renamed"
	if r.Kind == RuleMove {
		action = "moved"
	}
	since := r.Since
	if since == "" {
		since = "an unknown version"
	}
	msg := fmt.Sprintf("deprecated key %q was %s to %q since %s; please update your config", r.From, action, r.To, since)
	if r.Reason != "" {
		msg += ": " + r.Reason
	}
	return msg
}

func dropMessage(r Rule) string {
	since := r.Since
	if since == "" {
		since = "an unknown version"
	}
	msg := fmt.Sprintf("deprecated key %q was removed in %s and has no effect; please remove it", r.From, since)
	if r.Reason != "" {
		msg += ": " + r.Reason
	}
	return msg
}
