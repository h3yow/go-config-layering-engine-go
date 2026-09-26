package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Type 是 Schema 支持的字段类型。
type Type string

const (
	TypeString      Type = "string"
	TypeInt         Type = "int"
	TypeFloat       Type = "float"
	TypeBool        Type = "bool"
	TypeDuration    Type = "duration"
	TypeStringSlice Type = "strings"
)

// Field 描述单个配置字段的约束。
type Field struct {
	Type     Type
	Required bool
	// Default 为字段默认值（字符串形式，须可通过类型与约束校验）。
	Default string
	// Enum 非空时取值必须属于该集合。
	Enum []string
	// Min/Max 仅对 int/float/duration 生效（duration 使用 time.ParseDuration 可解析的字符串）。
	Min, Max string
	// Pattern 仅对 string 生效。
	Pattern string
	// Deprecated 非空时，任何来源为该字段赋值都会产生弃用提示。
	Deprecated string

	patternRe *regexp.Regexp
}

// Schema 是一组字段约束与兼容规则。
type Schema struct {
	Fields map[string]Field
	Rules  []Rule
}

// NewSchema 构造空 Schema。
func NewSchema() *Schema {
	return &Schema{Fields: map[string]Field{}}
}

// Add 注册字段约束；同名字段重复注册会被拒绝。
func (s *Schema) Add(path string, f Field) error {
	if s.Fields == nil {
		s.Fields = map[string]Field{}
	}
	path = canonical(path)
	if _, dup := s.Fields[path]; dup {
		return fmt.Errorf("schema: duplicate field %q", path)
	}
	s.Fields[path] = f
	return nil
}

// MustAdd 同 Add，但出错时 panic，便于声明式建表。
func (s *Schema) MustAdd(path string, f Field) *Schema {
	if err := s.Add(path, f); err != nil {
		panic(err)
	}
	return s
}

// AddRule 追加兼容规则。
func (s *Schema) AddRule(r Rule) *Schema {
	r.From = canonical(r.From)
	r.To = canonical(r.To)
	s.Rules = append(s.Rules, r)
	return s
}

// validate 检查 Schema 自身一致性：类型已知、默认值与约束可解析且合法、
// 规则引用的路径合理。构造 Manager 时调用，失败则拒绝启动。
func (s *Schema) validate() error {
	if len(s.Fields) == 0 {
		return fmt.Errorf("schema: no fields declared")
	}
	for path, f := range s.Fields {
		if err := s.validateField(path, &f); err != nil {
			return err
		}
		s.Fields[path] = f // 写回编译后的正则等缓存
	}
	for i, r := range s.Rules {
		switch r.Kind {
		case RuleRename:
			if r.From == "" || r.To == "" {
				return fmt.Errorf("schema: rule %d rename requires From and To", i)
			}
			if r.From == r.To {
				return fmt.Errorf("schema: rule %d rename %q onto itself", i, r.From)
			}
			if _, ok := s.Fields[r.To]; !ok {
				return fmt.Errorf("schema: rule %d rename target %q is not a declared field", i, r.To)
			}
			if _, ok := s.Fields[r.From]; ok {
				return fmt.Errorf("schema: rule %d rename source %q is still a declared field", i, r.From)
			}
		case RuleDelete:
			if r.From == "" {
				return fmt.Errorf("schema: rule %d delete requires From", i)
			}
			if r.OnDelete != DeleteReject && r.OnDelete != DeleteDropWarn {
				return fmt.Errorf("schema: rule %d delete %q has unknown policy %q", i, r.From, r.OnDelete)
			}
			if _, ok := s.Fields[r.From]; ok {
				return fmt.Errorf("schema: rule %d deletes %q which is still a declared field", i, r.From)
			}
		default:
			return fmt.Errorf("schema: rule %d has unknown kind %q", i, r.Kind)
		}
	}
	return nil
}

// validateField 校验单个字段声明，并编译正则缓存。
func (s *Schema) validateField(path string, f *Field) error {
	switch f.Type {
	case TypeString, TypeInt, TypeFloat, TypeBool, TypeDuration, TypeStringSlice:
	default:
		return fmt.Errorf("schema: field %q has unknown type %q", path, f.Type)
	}
	if f.Pattern != "" {
		if f.Type != TypeString {
			return fmt.Errorf("schema: field %q: Pattern only applies to string type", path)
		}
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return fmt.Errorf("schema: field %q: bad pattern: %v", path, err)
		}
		f.patternRe = re
	}
	if (f.Min != "" || f.Max != "") && f.Type != TypeInt && f.Type != TypeFloat && f.Type != TypeDuration {
		return fmt.Errorf("schema: field %q: Min/Max only apply to int/float/duration", path)
	}
	if f.Min != "" {
		if _, err := parseBound(f.Type, f.Min); err != nil {
			return fmt.Errorf("schema: field %q: bad Min: %v", path, err)
		}
	}
	if f.Max != "" {
		if _, err := parseBound(f.Type, f.Max); err != nil {
			return fmt.Errorf("schema: field %q: bad Max: %v", path, err)
		}
	}
	for _, e := range f.Enum {
		if err := checkType(f.Type, e); err != nil {
			return fmt.Errorf("schema: field %q: enum value %q: %v", path, e, err)
		}
	}
	if f.Default != "" {
		if err := checkValue(*f, path, "schema-default", f.Default); err != nil {
			return fmt.Errorf("schema: field %q: default not valid: %v", path, err)
		}
	}
	return nil
}

// parseBound 按类型解析边界值。
func parseBound(t Type, v string) (float64, error) {
	switch t {
	case TypeInt:
		n, err := strconv.ParseInt(v, 10, 64)
		return float64(n), err
	case TypeFloat:
		return strconv.ParseFloat(v, 64)
	case TypeDuration:
		d, err := time.ParseDuration(v)
		return float64(d), err
	default:
		return 0, fmt.Errorf("type %s has no ordering bounds", t)
	}
}

// checkType 仅校验值能否按类型解析，不检查约束。
func checkType(t Type, v string) error {
	switch t {
	case TypeString, TypeStringSlice:
		return nil
	case TypeInt:
		_, err := strconv.ParseInt(v, 10, 64)
		return err
	case TypeFloat:
		_, err := strconv.ParseFloat(v, 64)
		return err
	case TypeBool:
		_, err := strconv.ParseBool(v)
		return err
	case TypeDuration:
		_, err := time.ParseDuration(v)
		return err
	default:
		return fmt.Errorf("unknown type %q", t)
	}
}

// checkValue 校验值是否满足字段的类型与全部约束。
// 返回的 *Error 携带可区分原因：类型不匹配 / 取值非法。
func checkValue(f Field, path, source, v string) error {
	if err := checkType(f.Type, v); err != nil {
		return errf(ReasonTypeMismatch, path, source,
			"value %q is not a valid %s: %v", v, f.Type, err)
	}
	if len(f.Enum) > 0 {
		ok := false
		for _, e := range f.Enum {
			if v == e {
				ok = true
				break
			}
		}
		if !ok {
			return errf(ReasonInvalidValue, path, source,
				"value %q not in enum %v", v, f.Enum)
		}
	}
	if f.patternRe != nil && !f.patternRe.MatchString(v) {
		return errf(ReasonInvalidValue, path, source,
			"value %q does not match pattern %q", v, f.Pattern)
	}
	if f.Min != "" || f.Max != "" {
		num, err := parseBound(f.Type, v)
		if err != nil {
			return errf(ReasonTypeMismatch, path, source,
				"value %q is not a valid %s: %v", v, f.Type, err)
		}
		if f.Min != "" {
			lo, _ := parseBound(f.Type, f.Min)
			if num < lo {
				return errf(ReasonInvalidValue, path, source,
					"value %q below min %q", v, f.Min)
			}
		}
		if f.Max != "" {
			hi, _ := parseBound(f.Type, f.Max)
			if num > hi {
				return errf(ReasonInvalidValue, path, source,
					"value %q above max %q", v, f.Max)
			}
		}
	}
	return nil
}

// canonical 将点分路径规范化（小写、去空白段）。
func canonical(path string) string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return strings.Join(parts, ".")
}
