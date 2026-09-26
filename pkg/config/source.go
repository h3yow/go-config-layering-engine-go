package config

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Source 从某一来源读取扁平化的配置键值（点分路径 -> 字符串）。
// 返回的 map 中不应包含空键；实现须可重复调用。
type Source interface {
	// Name 返回来源的稳定标识，用于错误归因与解释输出。
	Name() string
	// Priority 返回来源优先级，数值越大优先级越高。
	Priority() int
	// Load 读取当前来源快照。
	Load(ctx context.Context) (map[string]string, error)
}

// 内置来源的默认优先级（越大越优先）。
const (
	PriorityDefaults = 0
	PriorityFile     = 100
	PriorityEnv      = 200
	PriorityOverride = 300
)

// MapSource 是内存型来源，常用于默认值与运行期覆盖。
type MapSource struct {
	name     string
	priority int
	values   map[string]string
}

// NewMapSource 构造内存来源；values 会被拷贝，后续修改入参不影响来源。
func NewMapSource(name string, priority int, values map[string]string) *MapSource {
	cp := make(map[string]string, len(values))
	for k, v := range values {
		cp[canonical(k)] = v
	}
	return &MapSource{name: name, priority: priority, values: cp}
}

func (s *MapSource) Name() string  { return s.name }
func (s *MapSource) Priority() int { return s.priority }

func (s *MapSource) Load(context.Context) (map[string]string, error) {
	cp := make(map[string]string, len(s.values))
	for k, v := range s.values {
		cp[k] = v
	}
	return cp, nil
}

// Set 更新内存来源中的单个键（对 Override 场景有用）。
// 注意：对已注册进 Manager 的来源调用 Set 不是并发安全的，
// 应在 Reload 之外、无并发读取时调用。
func (s *MapSource) Set(key, value string) { s.values[canonical(key)] = value }

// FileSource 从 JSON 文件读取配置，嵌套对象被展开为点分路径。
// 标量数组以逗号连接为字符串（对应 TypeStringSlice）。
type FileSource struct {
	name     string
	priority int
	path     string
}

// NewFileSource 构造文件来源。
func NewFileSource(name string, priority int, path string) *FileSource {
	return &FileSource{name: name, priority: priority, path: path}
}

func (s *FileSource) Name() string  { return s.name }
func (s *FileSource) Priority() int { return s.priority }

func (s *FileSource) Load(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, errf(ReasonSourceError, "", s.name, "read file: %v", err)
	}
	var root any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, errf(ReasonSourceError, "", s.name, "parse json: %v", err)
	}
	out := map[string]string{}
	if err := flattenJSON(s.name, "", root, out); err != nil {
		return nil, err
	}
	return out, nil
}

// flattenJSON 将 JSON 值展开为点分路径键。
func flattenJSON(src, prefix string, v any, out map[string]string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			key := canonical(k)
			if prefix != "" {
				key = prefix + "." + key
			}
			if err := flattenJSON(src, key, sub, out); err != nil {
				return err
			}
		}
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			s, err := scalarString(src, prefix, e)
			if err != nil {
				return err
			}
			parts = append(parts, s)
		}
		out[prefix] = strings.Join(parts, ",")
	case nil:
		// null 视为未设置，跳过
	default:
		s, err := scalarString(src, prefix, t)
		if err != nil {
			return err
		}
		out[prefix] = s
	}
	return nil
}

func scalarString(src, path string, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	default:
		return "", errf(ReasonTypeMismatch, path, src,
			"unsupported JSON value of type %T (nested arrays/objects inside arrays are not supported)", v)
	}
}

// EnvSource 从环境变量读取配置：按 prefix 过滤，
// 将 PREFIX_DB_HOST 形式的变量映射为 db.host（小写、下划线转点）。
type EnvSource struct {
	name     string
	priority int
	prefix   string
	environ  func() []string
}

// EnvOption 自定义 EnvSource 行为。
type EnvOption func(*EnvSource)

// WithEnviron 注入自定义环境变量枚举函数（便于测试）。
func WithEnviron(fn func() []string) EnvOption {
	return func(s *EnvSource) { s.environ = fn }
}

// NewEnvSource 构造环境变量来源；prefix 为空时读取全部环境变量。
func NewEnvSource(name string, priority int, prefix string, opts ...EnvOption) *EnvSource {
	s := &EnvSource{
		name:     name,
		priority: priority,
		prefix:   strings.ToUpper(strings.TrimSuffix(prefix, "_")),
		environ:  os.Environ,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *EnvSource) Name() string  { return s.name }
func (s *EnvSource) Priority() int { return s.priority }

func (s *EnvSource) Load(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	prefix := s.prefix
	if prefix != "" {
		prefix += "_"
	}
	for _, kv := range s.environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		mapped := strings.ToLower(strings.TrimPrefix(key, prefix))
		mapped = strings.ReplaceAll(mapped, "_", ".")
		if mapped == "" {
			continue
		}
		out[mapped] = val
	}
	return out, nil
}

// sortSourcesDesc 按优先级降序排序（稳定），供合并使用。
func sortSourcesDesc(sources []Source) []Source {
	out := append([]Source(nil), sources...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority() > out[j].Priority()
	})
	return out
}
