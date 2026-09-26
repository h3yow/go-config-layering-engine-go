package config

import (
	"fmt"
	"sort"
	"strings"
)

// mergeLayers 按 layers 的顺序（后者优先级更高）递归合并配置层。
// 返回合并树以及每个叶子路径的生效来源名；对象与非对象在同一路径冲突
// 时返回 KindTypeMismatch，绝不静默丢弃任一侧。
func mergeLayers(layers []RawLayer) (RawConfig, map[string]string, error) {
	result := RawConfig{}
	sources := map[string]string{}
	for _, layer := range layers {
		if err := mergeNode(result, map[string]any(layer.Data), layer.Name, nil, sources); err != nil {
			return nil, nil, err
		}
	}
	return result, sources, nil
}

func mergeNode(dst, src map[string]any, sourceName string, prefix []string, sources map[string]string) error {
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		path := appendPath(prefix, k)
		v := src[k]
		existing, exists := dst[k]

		switch vt := v.(type) {
		case map[string]any:
			if exists {
				em, ok := existing.(map[string]any)
				if !ok {
					return conflictErr(path, sourceName, "object", kindOf(existing))
				}
				if err := mergeNode(em, vt, sourceName, path, sources); err != nil {
					return err
				}
			} else {
				cloned := cloneValue(vt).(map[string]any)
				dst[k] = cloned
				registerLeaves(sources, cloned, path, sourceName)
			}
		default:
			// 标量、list、null 都视为叶子。
			if exists {
				if _, ok := existing.(map[string]any); ok {
					return conflictErr(path, sourceName, kindOf(v), "object")
				}
			}
			dst[k] = cloneValue(v)
			sources[strings.Join(path, ".")] = sourceName
		}
	}
	return nil
}

// registerLeaves 为新插入的整棵子树登记叶子来源。
func registerLeaves(sources map[string]string, node map[string]any, prefix []string, name string) {
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := appendPath(prefix, k)
		switch v := node[k].(type) {
		case map[string]any:
			registerLeaves(sources, v, path, name)
		default:
			sources[strings.Join(path, ".")] = name
		}
	}
}

func appendPath(prefix []string, seg string) []string {
	out := make([]string, 0, len(prefix)+1)
	out = append(out, prefix...)
	return append(out, seg)
}

func conflictErr(path []string, source string, incoming, existing string) error {
	return &ResolutionError{
		Kind:   KindTypeMismatch,
		Path:   strings.Join(path, "."),
		Source: source,
		Message: fmt.Sprintf(
			"cannot merge %s from source %q over existing %s at same path",
			incoming, source, existing),
	}
}
