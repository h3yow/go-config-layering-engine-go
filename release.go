package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// DiffKind 标识一条配置差异的类型。
type DiffKind string

const (
	// DiffAdded 候选中出现、当前生效配置中不存在的路径。
	DiffAdded DiffKind = "added"
	// DiffChanged 两侧都存在但取值不同的路径。
	DiffChanged DiffKind = "changed"
	// DiffRemoved 当前生效配置中存在、候选中被删掉的路径。
	DiffRemoved DiffKind = "removed"
)

// DiffEntry 是候选与当前生效配置之间的一条逐路径差异。
type DiffEntry struct {
	Path      string
	Kind      DiffKind
	OldValue  any
	NewValue  any
	OldSource string
	NewSource string
}

// Rejection 说明候选不被接受的一条原因：哪个字段、哪层来源、为什么。
type Rejection struct {
	Kind    ErrorKind
	Path    string
	Source  string
	Message string
}

// Preview 是一次发布前预演的结论。Accepted 为 false 时 Rejections 给出
// 逐条拒绝原因；Accepted 为 true 时 Candidate 与 Diff 描述将要发布的内容。
// Fingerprint 绑定预演时刻的来源内容，正式发布时据此核对"预演过的"与
// "要发布的"是否是同一份。
type Preview struct {
	ID           string
	BaseRevision int64
	Fingerprint  string
	Accepted     bool
	Candidate    *Resolved
	Diff         []DiffEntry
	Rejections   []Rejection
	CreatedAt    time.Time
}

// PreviewRequest 携带预演的并发控制信息，语义与 ReloadRequest 一致。
type PreviewRequest struct {
	ExpectedRevision int64
	RequestID        string
}

// PublishRequest 把一次已通过的预演正式发布生效。
//   - PreviewID：Preview.ID；
//   - ExpectedRevision：调用方认为当前生效的版本号；
//   - RequestID：非空时作为幂等键，重复提交被拒绝。
type PublishRequest struct {
	PreviewID        string
	ExpectedRevision int64
	RequestID        string
}

// Preview 在不影响当前生效配置的前提下，完整加载并校验候选配置，
// 与当前生效快照逐路径对比后返回结论。来源加载或校验失败不会返回
// Go 错误，而是体现为 Accepted=false 与逐条 Rejection；只有过期/
// 重复/未初始化等请求级问题才返回 Go 错误。
func (m *Manager) Preview(ctx context.Context, req PreviewRequest) (*Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.revision == 0 {
		return nil, fmt.Errorf("config: manager not initialized; call LoadInitial first")
	}
	if req.ExpectedRevision != m.revision {
		return nil, &StaleReloadError{Expected: req.ExpectedRevision, Current: m.revision}
	}
	if err := m.claimRequestIDLocked(req.RequestID); err != nil {
		return nil, err
	}

	pv := &Preview{BaseRevision: m.revision, CreatedAt: time.Now()}

	layers, err := m.loadLayersLocked(ctx)
	if err != nil {
		pv.Rejections = rejectionsFromError(err)
		return m.registerPreviewLocked(pv), nil
	}
	pv.Fingerprint = fingerprintLayers(layers)

	resolved, err := m.engine.Resolve(layers)
	if err != nil {
		pv.Rejections = rejectionsFromError(err)
		return m.registerPreviewLocked(pv), nil
	}

	pv.Accepted = true
	pv.Candidate = resolved
	pv.Diff = diffResolved(m.current.Load(), resolved)
	return m.registerPreviewLocked(pv), nil
}

// Publish 把预演通过的候选正式发布：重新读取来源并核对指纹，
// 来源内容在预演与发布之间发生变化时拒绝发布；一致则原子生效并记录版本历史。
func (m *Manager) Publish(ctx context.Context, req PublishRequest) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.revision == 0 {
		return nil, fmt.Errorf("config: manager not initialized; call LoadInitial first")
	}
	if err := m.claimRequestIDLocked(req.RequestID); err != nil {
		return nil, err
	}
	pv, ok := m.previews[req.PreviewID]
	if !ok {
		return nil, &UnknownPreviewError{PreviewID: req.PreviewID}
	}
	if req.ExpectedRevision != m.revision {
		return nil, &StaleReloadError{Expected: req.ExpectedRevision, Current: m.revision}
	}
	if pv.BaseRevision != m.revision {
		return nil, &StalePreviewError{BaseRevision: pv.BaseRevision, Current: m.revision}
	}
	if !pv.Accepted {
		return nil, &PreviewRejectedError{PreviewID: pv.ID, Rejections: pv.Rejections}
	}

	// 重新读取来源并核对指纹：预演与发布之间内容变了就必须重新预演，
	// 绝不把"预演通过"套用到变化后的内容上。
	layers, err := m.loadLayersLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish of preview %q failed at revision %d; previous config retained: %w",
			pv.ID, m.revision, err)
	}
	if fp := fingerprintLayers(layers); fp != pv.Fingerprint {
		return nil, &PreviewMismatchError{PreviewID: pv.ID}
	}

	// 指纹一致 => 解析流水线是确定性的，预演结论即发布内容；
	// 拷贝一份再赋新版本号，不改动预演对象本身。
	// 预演对象保留在登记表中（数量有界）：之后对它的发布会得到
	// StalePreviewError，而不是含义模糊的"未知预演"。
	candidate := *pv.Candidate
	return m.activateLocked(&candidate, VersionPublish, "publish "+pv.ID), nil
}

// diffResolved 逐路径对比当前生效快照与候选解析结果，输出按路径字典序
// 排序的差异列表；每条差异携带两侧各自的生效来源名。old 为 nil 时
// 候选的全部路径都视为新增。
func diffResolved(old, new *Resolved) []DiffEntry {
	var out []DiffEntry
	seen := map[string]bool{}
	var paths []string
	if old != nil {
		for p := range old.Values {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	for p := range new.Values {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		var oldVal, newVal any
		var oldSrc, newSrc string
		var hasOld, hasNew bool
		if old != nil {
			oldVal, hasOld = old.Values[p]
			oldSrc = old.Sources[p]
		}
		newVal, hasNew = new.Values[p]
		newSrc = new.Sources[p]

		switch {
		case !hasOld:
			out = append(out, DiffEntry{Path: p, Kind: DiffAdded, NewValue: newVal, NewSource: newSrc})
		case !hasNew:
			out = append(out, DiffEntry{Path: p, Kind: DiffRemoved, OldValue: oldVal, OldSource: oldSrc})
		case !reflect.DeepEqual(oldVal, newVal):
			out = append(out, DiffEntry{
				Path: p, Kind: DiffChanged,
				OldValue: oldVal, NewValue: newVal,
				OldSource: oldSrc, NewSource: newSrc,
			})
		}
	}
	return out
}

// fingerprintLayers 对加载到的原始层（含顺序与来源名）计算内容指纹。
// json.Marshal 对 map 键排序，因此同一内容必然得到同一指纹；
// 预演与正式发布各自计算一次，不一致即说明两次之间来源内容变了。
func fingerprintLayers(layers []RawLayer) string {
	h := sha256.New()
	for _, l := range layers {
		b, err := json.Marshal(l.Data)
		if err != nil {
			// RawConfig 只允许 JSON 风格类型，正常不会失败；失败时退化为 fmt 形式也要保证确定性。
			b = []byte(fmt.Sprintf("%#v", l.Data))
		}
		h.Write([]byte(l.Name))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rejectionsFromError 把加载/校验失败转成逐条 Rejection，
// 保留 ResolutionError 的 Kind/Path/Source 定位信息。
func rejectionsFromError(err error) []Rejection {
	if re, ok := AsResolutionError(err); ok {
		return []Rejection{{Kind: re.Kind, Path: re.Path, Source: re.Source, Message: re.Message}}
	}
	return []Rejection{{Kind: KindSourceLoad, Message: err.Error()}}
}
