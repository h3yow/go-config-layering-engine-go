package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Source 是一个配置来源：每次 Load 产出当前时刻的原始配置树。
type Source interface {
	Name() string
	Load(ctx context.Context) (RawConfig, error)
}

// StaticSource 是内存静态来源，适合默认值与测试夹具。
type StaticSource struct {
	name string
	data RawConfig
}

// NewStaticSource 构造内存静态来源。
func NewStaticSource(name string, data RawConfig) Source {
	return &StaticSource{name: name, data: cloneRaw(data)}
}

func (s *StaticSource) Name() string { return s.name }

func (s *StaticSource) Load(_ context.Context) (RawConfig, error) {
	return cloneRaw(s.data), nil
}

// OverrideSource 是可在运行期替换内容的最高优先级覆盖来源；并发安全。
type OverrideSource struct {
	mu   sync.RWMutex
	name string
	data RawConfig
}

// NewOverrideSource 创建运行期覆盖来源。
func NewOverrideSource(name string, initial RawConfig) *OverrideSource {
	return &OverrideSource{name: name, data: cloneRaw(initial)}
}

func (o *OverrideSource) Name() string { return o.name }

func (o *OverrideSource) Load(_ context.Context) (RawConfig, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return cloneRaw(o.data), nil
}

// Set 原子替换覆盖内容。
func (o *OverrideSource) Set(data RawConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = cloneRaw(data)
}

// FileSource 从 JSON 文件加载配置，每次重载重新读取。
type FileSource struct {
	path string
	name string
}

// NewFileSource 创建 JSON 文件来源。
func NewFileSource(name, path string) *FileSource {
	return &FileSource{name: name, path: path}
}

func (f *FileSource) Name() string { return f.name }

func (f *FileSource) Load(ctx context.Context) (RawConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, &ResolutionError{
			Kind:    KindSourceLoad,
			Source:  f.name,
			Message: fmt.Sprintf("read config file %q: %v", f.path, err),
			Cause:   err,
		}
	}
	var tree map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, &ResolutionError{
			Kind:    KindSourceLoad,
			Source:  f.name,
			Message: fmt.Sprintf("parse JSON config file %q: %v", f.path, err),
			Cause:   err,
		}
	}
	if err := normalizeJSONNumbers(map[string]any(tree)); err != nil {
		return nil, &ResolutionError{Kind: KindSourceLoad, Source: f.name, Message: err.Error(), Cause: err}
	}
	return RawConfig(tree), nil
}

// normalizeJSONNumbers 把 json.Number 按"是否为整数"确定性转成 int64/float64，
// 避免类型系统中出现 schema 未知的 json.Number。
func normalizeJSONNumbers(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch n := val.(type) {
			case json.Number:
				cv, err := decodeJSONNumber(n)
				if err != nil {
					return err
				}
				t[k] = cv
			default:
				if err := normalizeJSONNumbers(val); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, val := range t {
			switch n := val.(type) {
			case json.Number:
				cv, err := decodeJSONNumber(n)
				if err != nil {
					return err
				}
				t[i] = cv
			default:
				if err := normalizeJSONNumbers(val); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeJSONNumber(n json.Number) (any, error) {
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil, fmt.Errorf("invalid number %q: %w", n.String(), err)
	}
	return f, nil
}

// EnvSource 按 schema 中的 Env 绑定从环境变量加载配置。
type EnvSource struct {
	name   string
	schema *Schema
	lookup func(string) (string, bool)
}

// NewEnvSource 创建环境变量来源；lookup 为 nil 时使用 os.LookupEnv。
func NewEnvSource(name string, schema *Schema, lookup func(string) (string, bool)) *EnvSource {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &EnvSource{name: name, schema: schema, lookup: lookup}
}

func (e *EnvSource) Name() string { return e.name }

func (e *EnvSource) Load(ctx context.Context) (RawConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tree := RawConfig{}
	for i := range e.schema.Fields {
		f := &e.schema.Fields[i]
		if f.Env == "" {
			continue
		}
		rawStr, ok := e.lookup(f.Env)
		if !ok {
			continue
		}
		var raw any = rawStr
		if f.Type == TypeList {
			raw = splitCSVEnv(rawStr)
		}
		v, err := coerceScalar(f, raw)
		if err != nil {
			return nil, &ResolutionError{
				Kind:   KindSourceLoad,
				Source: e.name,
				Path:   f.Path,
				Message: fmt.Sprintf("environment variable %s for %q rejected: %s",
					f.Env, f.Path, flattenErrMsg(err)),
				Cause: err,
			}
		}
		if err := setPath(tree, f.Path, v); err != nil {
			return nil, &ResolutionError{
				Kind:    KindSourceLoad,
				Source:  e.name,
				Path:    f.Path,
				Message: err.Error(),
				Cause:   err,
			}
		}
	}
	return tree, nil
}

// splitCSVEnv 把逗号分隔的环境变量值切为字符串列表，空字符串视为空列表。
func splitCSVEnv(s string) []any {
	if strings.TrimSpace(s) == "" {
		return []any{}
	}
	parts := strings.Split(s, ",")
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

func flattenErrMsg(err error) string {
	if re, ok := AsResolutionError(err); ok {
		return re.Message
	}
	return err.Error()
}
