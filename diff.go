package config

import "sort"

// ChangeKind 标识两条生效配置之间一条叶子路径的差异类别。
type ChangeKind string

const (
	// ChangeAdded 候选中新增的路径（当前生效配置没有）。
	ChangeAdded ChangeKind = "added"
	// ChangeChanged 路径两边都存在但取值不同。
	ChangeChanged ChangeKind = "changed"
	// ChangeRemoved 候选中消失的路径（当前生效配置有）。
	ChangeRemoved ChangeKind = "removed"
)

// Change 是逐叶子路径的一条差异结论，附带两侧各自的生效来源，
// 用于解释"这个值为什么变 / 从哪一层来"。
type Change struct {
	Kind      ChangeKind
	Path      string
	OldValue  any    // changed/removed 时有值
	NewValue  any    // added/changed 时有值
	OldSource string // 当前生效配置中该路径的来源；added 时为空
	NewSource string // 候选配置中该路径的来源；removed 时为空
}

// DiffResolved 对比两份解析结果，按路径字典序返回全部叶子差异。
// old 为 nil 时视为空配置（全部为 added）。输入被视为不可变，不会被修改。
func DiffResolved(old, new *Resolved) []Change {
	var oldValues, newValues map[string]any
	var oldSources, newSources map[string]string
	if old != nil {
		oldValues, oldSources = old.Values, old.Sources
	}
	if new != nil {
		newValues, newSources = new.Values, new.Sources
	}

	pathSet := map[string]struct{}{}
	for p := range oldValues {
		pathSet[p] = struct{}{}
	}
	for p := range newValues {
		pathSet[p] = struct{}{}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var changes []Change
	for _, p := range paths {
		ov, inOld := oldValues[p]
		nv, inNew := newValues[p]
		switch {
		case !inOld && inNew:
			changes = append(changes, Change{
				Kind: ChangeAdded, Path: p, NewValue: nv, NewSource: newSources[p],
			})
		case inOld && !inNew:
			changes = append(changes, Change{
				Kind: ChangeRemoved, Path: p, OldValue: ov, OldSource: oldSources[p],
			})
		case !canonicalEqual(ov, nv):
			changes = append(changes, Change{
				Kind:      ChangeChanged,
				Path:      p,
				OldValue:  ov,
				NewValue:  nv,
				OldSource: oldSources[p],
				NewSource: newSources[p],
			})
		}
	}
	return changes
}

// canonicalEqual 比较两个规范化后的叶子值（string/int64/float64/bool/[]any），
// 列表逐元素递归比较；标量类型已在解析期规范化，可直接比较。
func canonicalEqual(a, b any) bool {
	if al, ok := a.([]any); ok {
		bl, ok := b.([]any)
		if !ok || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !canonicalEqual(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}
