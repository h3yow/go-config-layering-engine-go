package config

import (
	"fmt"
	"sort"
	"strings"
)

// RawConfig 是各来源产出的原始配置树，仅允许 JSON 风格的类型：
// map[string]any、[]any、string、bool、float64、nil。
type RawConfig map[string]any

// RawLayer 是带来源名称的一层配置。
type RawLayer struct {
	Name string
	Data RawConfig
}

// kindOf 返回值的稳定类型名称，用于类型不匹配错误。
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case int:
		return "int"
	case int64:
		return "int"
	case float64:
		return "number"
	case []any:
		return "list"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// splitPath 把点分路径切为段，转义形式 "a\.b" 中的点不切分。
func splitPath(path string) []string {
	var segs []string
	var cur strings.Builder
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '\\':
			if i+1 < len(path) {
				cur.WriteByte(path[i+1])
				i++
			}
		case '.':
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(path[i])
		}
	}
	segs = append(segs, cur.String())
	return segs
}

// joinPath 与 splitPath 对偶（仅用于错误展示，不做转义还原）。
func joinPath(segs []string) string { return strings.Join(segs, ".") }

// getPath 沿点分路径读取叶子或中间节点；路径上类型错误时 ok=false。
func getPath(tree RawConfig, path string) (any, bool) {
	segs := splitPath(path)
	var cur any = map[string]any(tree)
	for _, seg := range segs {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath 沿点分路径写入值，缺失的中间对象按需创建；
// 若中间路径已被非对象占用则返回错误。
func setPath(tree RawConfig, path string, value any) error {
	segs := splitPath(path)
	var cur map[string]any = map[string]any(tree)
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = value
			return nil
		}
		next, exists := cur[seg]
		if !exists {
			m := map[string]any{}
			cur[seg] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return &ResolutionError{
				Kind:    KindTypeMismatch,
				Path:    joinPath(segs[:i+1]),
				Message: fmt.Sprintf("expected object to descend into, got %s", kindOf(next)),
			}
		}
		cur = m
	}
	return nil
}

// deletePath 删除叶子；若删除后父对象为空则一并清理，返回是否曾存在。
func deletePath(tree RawConfig, path string) bool {
	segs := splitPath(path)
	stack := []map[string]any{map[string]any(tree)}
	cur := map[string]any(tree)
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return false
		}
		stack = append(stack, next)
		cur = next
	}
	last := segs[len(segs)-1]
	if _, ok := cur[last]; !ok {
		return false
	}
	delete(cur, last)
	// 清理向上变为空的父对象。
	for i := len(stack) - 1; i >= 1; i-- {
		if len(stack[i]) != 0 {
			break
		}
		delete(stack[i-1], segs[i-1])
	}
	return true
}

// flatten 将配置树摊平为点分叶子路径映射。叶子定义：
// 标量、null、list；map 视为容器。
func flatten(cfg RawConfig) map[string]any {
	out := map[string]any{}
	var walk func(prefix []string, v any)
	walk = func(prefix []string, v any) {
		if m, ok := v.(map[string]any); ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(append(prefix, k), m[k])
			}
			return
		}
		out[joinPath(prefix)] = v
	}
	walk(nil, map[string]any(cfg))
	return out
}

// cloneRaw 深拷贝配置树，保证来源数据与解析结果互不影响。
func cloneRaw(in RawConfig) RawConfig {
	if in == nil {
		return nil
	}
	return cloneValue(map[string]any(in)).(map[string]any)
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = cloneValue(val)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, val := range t {
			l[i] = cloneValue(val)
		}
		return l
	default:
		return v
	}
}
