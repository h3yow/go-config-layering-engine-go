package config

import "sort"

// resolved 是合并+校验后的单个字段结果。
type resolved struct {
	value  string
	source string
}

// Warning 是解析过程中的非致命提示（弃用、迁移等）。
type Warning struct {
	Path   string
	Source string
	Msg    string
}

// snapshot 是一份原子生效的配置快照。
type snapshot struct {
	version uint64
	vals    map[string]resolved
	warns   []Warning
}

// namedData 是带名字的一份来源数据。
type namedData struct {
	name string
	data map[string]string
}

// merge 按优先级从高到低合并各来源数据：高优先级先占位，
// 低优先级仅补充尚未出现的键。sources 须已按优先级降序排列。
func merge(sources []namedData) map[string]resolved {
	out := map[string]resolved{}
	for _, nd := range sources {
		for k, v := range nd.data {
			if _, taken := out[k]; taken {
				continue
			}
			out[k] = resolved{value: v, source: nd.name}
		}
	}
	return out
}

// validateAndFill 对合并结果按 Schema 校验并补齐默认值。
// 判定顺序固定（字段按路径排序），保证同一输入得到同一结论：
//  1. 合并结果中出现未声明字段 → ReasonInvalidValue（拒绝静默丢字段）；
//  2. 已声明字段：有来源值则校验类型与约束，失败给出
//     ReasonTypeMismatch / ReasonInvalidValue，绝不回退默认值；
//  3. 无来源值但有默认值 → 以 "default" 来源填入；
//  4. 无来源值且必填 → ReasonMissingRequired。
func validateAndFill(schema *Schema, merged map[string]resolved) (map[string]resolved, []Warning, error) {
	// 1. 拒绝未知字段
	unknown := make([]string, 0)
	for k := range merged {
		if _, ok := schema.Fields[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		k := unknown[0]
		return nil, nil, errf(ReasonInvalidValue, k, merged[k].source,
			"unknown field %q (not declared in schema and no migration rule matched)", k)
	}

	paths := make([]string, 0, len(schema.Fields))
	for p := range schema.Fields {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make(map[string]resolved, len(schema.Fields))
	var warns []Warning
	for _, p := range paths {
		f := schema.Fields[p]
		r, ok := merged[p]
		if ok {
			// 2. 校验来源值
			if err := checkValue(f, p, r.source, r.value); err != nil {
				return nil, nil, err
			}
			if f.Deprecated != "" {
				warns = append(warns, Warning{
					Path:   p,
					Source: r.source,
					Msg:    "field " + p + " is deprecated: " + f.Deprecated,
				})
			}
			out[p] = r
			continue
		}
		// 3. 默认值
		if f.Default != "" {
			out[p] = resolved{value: f.Default, source: "default"}
			continue
		}
		// 4. 必填缺失
		if f.Required {
			return nil, nil, errf(ReasonMissingRequired, p, "",
				"required field %q not provided by any source and has no default", p)
		}
	}
	return out, warns, nil
}
