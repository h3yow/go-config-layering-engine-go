package config

import (
	"fmt"
	"sort"
	"time"
)

// Engine 把"迁移 -> 分层合并 -> schema 校验"串成确定性解析流水线。
type Engine struct {
	schema *Schema
	rules  []Rule
}

// NewEngine 创建解析引擎；规则声明会先做自洽性检查。
func NewEngine(schema *Schema, rules []Rule) (*Engine, error) {
	if schema == nil {
		return nil, fmt.Errorf("config: engine requires a schema")
	}
	if err := validateRules(rules); err != nil {
		return nil, err
	}
	return &Engine{schema: schema, rules: rules}, nil
}

// Resolved 是一次解析的不可变结果。
type Resolved struct {
	// Revision 由 Manager 在生效时赋予的单调版本号。
	Revision int64
	// Tree 合并并规范化类型后的完整配置树。
	Tree RawConfig
	// Values 是点分路径 -> 规范类型叶子值。
	Values map[string]any
	// Sources 记录每个叶子路径的生效来源名，用于解释优先级结论。
	Sources map[string]string
	// Warnings 是弃用/迁移提示。
	Warnings []Warning
	// ResolvedAt 解析时间。
	ResolvedAt time.Time
}

// Resolve 执行完整解析流水线。layers 顺序即优先级从低到高；
// 输入层会被深拷贝，调用方后续修改不影响结果。
func (e *Engine) Resolve(layers []RawLayer) (*Resolved, error) {
	migrated := make([]RawLayer, len(layers))
	var warnings []Warning
	for i, layer := range layers {
		tree := cloneRaw(layer.Data)
		ws, err := applyMigrations(tree, e.rules)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			w.Message = fmt.Sprintf("[source=%s] %s", layer.Name, w.Message)
			warnings = append(warnings, w)
		}
		migrated[i] = RawLayer{Name: layer.Name, Data: tree}
	}

	merged, sources, err := mergeLayers(migrated)
	if err != nil {
		return nil, err
	}
	if err := e.validateShapes(merged, sources); err != nil {
		return nil, err
	}

	values, err := e.validateLeaves(merged, sources)
	if err != nil {
		return nil, err
	}
	if err := e.validateRequired(values); err != nil {
		return nil, err
	}

	canonical := RawConfig{}
	for path, v := range values {
		if err := setPath(canonical, path, v); err != nil {
			return nil, err
		}
	}

	return &Resolved{
		Tree:       canonical,
		Values:     values,
		Sources:    sources,
		Warnings:   warnings,
		ResolvedAt: time.Now(),
	}, nil
}

// validateShapes 检查 schema 叶子路径上是否被对象占用（对象/叶子冲突）。
func (e *Engine) validateShapes(tree RawConfig, sources map[string]string) error {
	paths := make([]string, 0, len(e.schema.byPath))
	for p := range e.schema.byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		v, exists := getPath(tree, p)
		if !exists {
			continue
		}
		if _, isObj := v.(map[string]any); isObj {
			return &ResolutionError{
				Kind:    KindTypeMismatch,
				Path:    p,
				Source:  sources[p],
				Message: fmt.Sprintf("expected scalar field %q, got object", p),
			}
		}
	}
	return nil
}

// validateLeaves 摊平合并树，逐叶子做未知字段、类型、枚举与范围校验，
// 返回规范化后的值映射；路径按字典序处理以保证错误可复现。
func (e *Engine) validateLeaves(tree RawConfig, sources map[string]string) (map[string]any, error) {
	flat := flatten(tree)
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	values := make(map[string]any, len(flat))
	for _, p := range paths {
		raw := flat[p]
		f, ok := e.schema.field(p)
		if !ok {
			return nil, &ResolutionError{
				Kind:    KindUnknownField,
				Path:    p,
				Source:  sources[p],
				Message: fmt.Sprintf("unknown field %q is not declared in schema %q and no migration rule covers it", p, e.schema.Name),
			}
		}
		if raw == nil {
			return nil, &ResolutionError{
				Kind:    KindTypeMismatch,
				Path:    p,
				Source:  sources[p],
				Message: fmt.Sprintf("field %q is null; expected %s", p, f.Type),
			}
		}
		v, err := coerceScalar(f, raw)
		if err != nil {
			return nil, decorate(err, p, sources[p])
		}
		if err := checkConstraints(f, v); err != nil {
			return nil, decorate(err, p, sources[p])
		}
		values[p] = v
	}
	return values, nil
}

func decorate(err error, path, source string) error {
	re, ok := AsResolutionError(err)
	if !ok {
		return err
	}
	re.Path = path
	re.Source = source
	return re
}

func checkConstraints(f *Field, v any) error {
	switch f.Type {
	case TypeInt, TypeFloat:
		x := numericValue(v)
		if f.Min != nil && x < *f.Min {
			return invalid(f, v, fmt.Sprintf("value %v is below minimum %g", v, *f.Min))
		}
		if f.Max != nil && x > *f.Max {
			return invalid(f, v, fmt.Sprintf("value %v is above maximum %g", v, *f.Max))
		}
		if len(f.Enum) > 0 && !inEnum(v, f.Enum) {
			return invalid(f, v, enumMsg(v, f.Enum))
		}
	case TypeList:
		items := v.([]any)
		if f.Min != nil && float64(len(items)) < *f.Min {
			return invalid(f, v, fmt.Sprintf("list length %d is below minimum %g", len(items), *f.Min))
		}
		if f.Max != nil && float64(len(items)) > *f.Max {
			return invalid(f, v, fmt.Sprintf("list length %d is above maximum %g", len(items), *f.Max))
		}
		if len(f.Enum) > 0 {
			for i, it := range items {
				if !inEnum(it, f.Enum) {
					return invalid(f, v, fmt.Sprintf("list element [%d] %v not allowed; %s", i, it, enumMsg(it, f.Enum)))
				}
			}
		}
	case TypeString, TypeBool:
		if len(f.Enum) > 0 && !inEnum(v, f.Enum) {
			return invalid(f, v, enumMsg(v, f.Enum))
		}
	}
	return nil
}

func invalid(f *Field, v any, msg string) error {
	return &ResolutionError{Kind: KindInvalidValue, Message: msg}
}

func numericValue(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

func inEnum(v any, enum []any) bool {
	for _, allowed := range enum {
		if valuesEqualLoose(v, allowed) {
			return true
		}
	}
	return false
}

// valuesEqualLoose 允许枚举用 Go 原生 int/float 字面量声明，与规范化后的值比较。
func valuesEqualLoose(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case int64:
		switch bv := b.(type) {
		case int:
			return av == int64(bv)
		case int64:
			return av == bv
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		}
	}
	return false
}

func enumMsg(v any, enum []any) string {
	return fmt.Sprintf("value %v is not one of the allowed values %v", v, enum)
}

func (e *Engine) validateRequired(values map[string]any) error {
	paths := make([]string, 0, len(e.schema.byPath))
	for p := range e.schema.byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, _ := e.schema.field(p)
		if f.Required {
			if _, ok := values[p]; !ok {
				return &ResolutionError{
					Kind:    KindMissingRequired,
					Path:    p,
					Message: fmt.Sprintf("required field %q is missing from every config source", p),
				}
			}
		}
	}
	return nil
}
