package config

import (
	"fmt"
)

// ScalarType 是字段支持的标量（及列表）类型名。
type ScalarType string

const (
	TypeString ScalarType = "string"
	TypeInt    ScalarType = "int"
	TypeFloat  ScalarType = "float"
	TypeBool   ScalarType = "bool"
	TypeList   ScalarType = "list"
)

// Field 声明一个配置叶子字段的校验规则。
type Field struct {
	// Path 点分路径，如 "server.port"。
	Path string
	// Type 叶子类型；TypeList 时用 ElemType 声明元素类型。
	Type ScalarType
	// ElemType 列表元素的标量类型。
	ElemType ScalarType
	// Required 为 true 时，所有来源合并后仍缺失即拒绝。
	Required bool
	// Default 字段默认值，类型须与 Type 一致（由默认来源装载）。
	Default any
	// Enum 非空时取值必须在枚举集合内。
	Enum []any
	// Min/Max 非 nil 时对 int/float 及列表长度做范围约束。
	Min *float64
	Max *float64
	// Env 绑定的环境变量名，空表示不绑定。
	Env string
	// Description 人类可读说明。
	Description string
}

// Schema 是一份字段声明集合。
type Schema struct {
	Name   string
	Fields []Field

	byPath map[string]int
}

// NewSchema 构造 schema 并建立路径索引；重复路径会被拒绝。
func NewSchema(name string, fields []Field) (*Schema, error) {
	s := &Schema{Name: name, Fields: fields, byPath: map[string]int{}}
	for i := range fields {
		f := &fields[i]
		if _, dup := s.byPath[f.Path]; dup {
			return nil, fmt.Errorf("config: schema %q has duplicate field path %q", name, f.Path)
		}
		if err := validateFieldShape(f); err != nil {
			return nil, err
		}
		s.byPath[f.Path] = i
	}
	return s, nil
}

func validateFieldShape(f *Field) error {
	switch f.Type {
	case TypeString, TypeInt, TypeFloat, TypeBool, TypeList:
	default:
		return fmt.Errorf("config: schema field %q has unknown type %q", f.Path, f.Type)
	}
	if f.Type == TypeList {
		switch f.ElemType {
		case TypeString, TypeInt, TypeFloat, TypeBool:
		default:
			return fmt.Errorf("config: schema list field %q has invalid elem type %q", f.Path, f.ElemType)
		}
	}
	if f.Default != nil {
		if _, err := coerceScalar(f, f.Default); err != nil {
			return fmt.Errorf("config: schema field %q default invalid: %w", f.Path, err)
		}
	}
	return nil
}

func (s *Schema) field(path string) (*Field, bool) {
	if s == nil {
		return nil, false
	}
	i, ok := s.byPath[path]
	if !ok {
		return nil, false
	}
	return &s.Fields[i], true
}

// coerceScalar 把原始值（通常来自 JSON/环境变量）转换为字段声明的类型。
// 转换失败返回 KindTypeMismatch，绝不静默回退。
func coerceScalar(f *Field, raw any) (any, error) {
	return coerceAs(f.Type, f.ElemType, raw)
}

func coerceAs(t ScalarType, elem ScalarType, raw any) (any, error) {
	switch t {
	case TypeString:
		v, ok := raw.(string)
		if !ok {
			return nil, typeErr(t, raw)
		}
		return v, nil
	case TypeBool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			// 环境变量路径允许常见写法；JSON 中的 bool 本就是 bool。
			switch v {
			case "true", "1", "yes", "on":
				return true, nil
			case "false", "0", "no", "off":
				return false, nil
			}
			return nil, typeErr(TypeBool, raw)
		default:
			return nil, typeErr(TypeBool, raw)
		}
	case TypeInt:
		return toInt(raw)
	case TypeFloat:
		return toFloat(raw)
	case TypeList:
		return toList(elem, raw)
	default:
		return nil, fmt.Errorf("config: unsupported scalar type %q", t)
	}
}

func typeErr(want ScalarType, got any) error {
	return &ResolutionError{
		Kind:    KindTypeMismatch,
		Message: fmt.Sprintf("expected %s, got %s (%v)", want, kindOf(got), got),
	}
}

func toInt(raw any) (any, error) {
	switch v := raw.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if v != float64(int64(v)) {
			return nil, &ResolutionError{Kind: KindTypeMismatch, Message: fmt.Sprintf("expected int, got fractional number %v", v)}
		}
		return int64(v), nil
	case string:
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || v == "" {
			return nil, &ResolutionError{Kind: KindTypeMismatch, Message: fmt.Sprintf("expected int, got string %q", v)}
		}
		return n, nil
	default:
		return nil, typeErr(TypeInt, raw)
	}
}

func toFloat(raw any) (any, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		var x float64
		if _, err := fmt.Sscanf(v, "%g", &x); err != nil || v == "" {
			return nil, &ResolutionError{Kind: KindTypeMismatch, Message: fmt.Sprintf("expected number, got string %q", v)}
		}
		return x, nil
	default:
		return nil, typeErr(TypeFloat, raw)
	}
}

// toList 接受 []any；字符串仅允许逗号分隔的字符串列表，其他标量不隐式包装。
func toList(elem ScalarType, raw any) (any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, typeErr(TypeList, raw)
	}
	out := make([]any, len(items))
	for i, it := range items {
		v, err := coerceAs(elem, "", it)
		if err != nil {
			return nil, &ResolutionError{
				Kind:    KindTypeMismatch,
				Message: fmt.Sprintf("list element [%d]: %s", i, err.Error()),
			}
		}
		out[i] = v
	}
	return out, nil
}

// defaultLayer 根据 schema 中声明的 Default 生成默认值来源。
func defaultLayer(schema *Schema) RawConfig {
	tree := RawConfig{}
	for i := range schema.Fields {
		f := &schema.Fields[i]
		if f.Default == nil {
			continue
		}
		v, err := coerceScalar(f, f.Default)
		if err != nil {
			// NewSchema 已校验，不会发生。
			continue
		}
		_ = setPath(tree, f.Path, v)
	}
	return tree
}
