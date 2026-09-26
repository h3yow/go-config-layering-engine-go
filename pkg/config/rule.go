package config

// RuleKind 兼容规则类型。
type RuleKind string

const (
	// RuleRename 字段改名或移动：旧路径的值迁移到新路径。
	RuleRename RuleKind = "rename"
	// RuleDelete 字段删除：按 OnDelete 策略处理残留值。
	RuleDelete RuleKind = "delete"
)

// 删除策略。
const (
	// DeleteReject 历史配置仍携带该字段时拒绝（默认，防止静默丢字段）。
	DeleteReject = "reject"
	// DeleteDropWarn 丢弃该字段并产生弃用提示。
	DeleteDropWarn = "drop_warn"
)

// Rule 描述一条结构演进兼容规则。
type Rule struct {
	Kind RuleKind
	// From 为历史路径（rename/delete 均使用）。
	From string
	// To 为 rename 的目标路径。
	To string
	// OnDelete 为 delete 规则的策略：DeleteReject 或 DeleteDropWarn。
	OnDelete string
	// Since 标记规则引入的版本，用于文档与提示。
	Since string
}

// Rename 声明字段改名/移动规则。
func Rename(from, to string) Rule {
	return Rule{Kind: RuleRename, From: from, To: to}
}

// Delete 声明字段删除规则；policy 为空时默认 DeleteReject。
func Delete(from, policy string) Rule {
	if policy == "" {
		policy = DeleteReject
	}
	return Rule{Kind: RuleDelete, From: from, OnDelete: policy}
}

// applyRules 对单来源的扁平数据按声明顺序应用兼容规则。
// 返回迁移后的数据与弃用提示。
//
// 判定依据（确定性：同一输入与规则集必得同一结论）：
//   - rename：旧路径存在且新路径不存在 → 迁移并提示弃用；
//     新旧同时存在且值相同 → 丢弃旧键并提示；值不同 → ReasonMigrationFailure；
//   - delete + reject：旧路径仍存在 → ReasonMigrationFailure；
//   - delete + drop_warn：丢弃旧键并提示。
func applyRules(schema *Schema, source string, data map[string]string) (map[string]string, []Warning, error) {
	if len(schema.Rules) == 0 {
		return data, nil, nil
	}
	out := make(map[string]string, len(data))
	for k, v := range data {
		out[k] = v
	}
	var warns []Warning
	for _, r := range schema.Rules {
		switch r.Kind {
		case RuleRename:
			oldVal, hasOld := out[r.From]
			if !hasOld {
				continue
			}
			newVal, hasNew := out[r.To]
			if hasNew && newVal != oldVal {
				return nil, nil, errf(ReasonMigrationFailure, r.From, source,
					"cannot migrate %q -> %q: both present with conflicting values (%q vs %q)",
					r.From, r.To, oldVal, newVal)
			}
			delete(out, r.From)
			if !hasNew {
				out[r.To] = oldVal
			}
			warns = append(warns, Warning{
				Path:   r.To,
				Source: source,
				Msg:    "field " + r.From + " migrated to " + r.To + deprecationSuffix(r),
			})
		case RuleDelete:
			if _, has := out[r.From]; !has {
				continue
			}
			if r.OnDelete == DeleteReject {
				return nil, nil, errf(ReasonMigrationFailure, r.From, source,
					"field %q was removed%s and cannot be migrated; please update this source",
					r.From, deprecationSuffix(r))
			}
			delete(out, r.From)
			warns = append(warns, Warning{
				Path:   r.From,
				Source: source,
				Msg:    "field " + r.From + " dropped" + deprecationSuffix(r),
			})
		}
	}
	return out, warns, nil
}

func deprecationSuffix(r Rule) string {
	if r.Since == "" {
		return " (deprecated)"
	}
	return " (deprecated since " + r.Since + ")"
}
