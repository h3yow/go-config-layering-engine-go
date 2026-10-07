package config

import (
	"errors"
	"fmt"
)

// ErrorKind 以稳定的字符串标识配置解析被拒绝的原因，便于调用方区分与断言。
type ErrorKind string

const (
	// KindSourceLoad 来源自身读取失败（文件不存在、环境变量值无法解析等）。
	KindSourceLoad ErrorKind = "source_load"
	// KindTypeMismatch 合并或校验时发现类型不匹配（如对象与标量冲突、int 字段得到字符串）。
	KindTypeMismatch ErrorKind = "type_mismatch"
	// KindMissingRequired 必填字段在所有来源中均缺失。
	KindMissingRequired ErrorKind = "missing_required"
	// KindInvalidValue 取值非法（越界、不在枚举内等），不得静默回退默认值。
	KindInvalidValue ErrorKind = "invalid_value"
	// KindUnknownField 历史配置中出现 schema 未声明且无迁移规则覆盖的字段。
	KindUnknownField ErrorKind = "unknown_field"
	// KindMigrationConflict 迁移规则无法安全应用（新旧路径同时存在等）。
	KindMigrationConflict ErrorKind = "migration_conflict"
)

// ResolutionError 是所有"配置被拒绝"场景的统一错误类型，
// Kind 可机读区分原因，Path/Source 说明定位，Message 给出判定依据。
type ResolutionError struct {
	Kind    ErrorKind
	Path    string
	Source  string
	Message string
	Cause   error
}

func (e *ResolutionError) Error() string {
	loc := e.Path
	if e.Source != "" {
		if loc != "" {
			loc += "@" + e.Source
		} else {
			loc = e.Source
		}
	}
	if loc != "" {
		return fmt.Sprintf("config: [%s] %s: %s", e.Kind, loc, e.Message)
	}
	return fmt.Sprintf("config: [%s] %s", e.Kind, e.Message)
}

func (e *ResolutionError) Unwrap() error { return e.Cause }

// AsResolutionError 从错误链中提取 *ResolutionError。
func AsResolutionError(err error) (*ResolutionError, bool) {
	var re *ResolutionError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// StaleReloadError 表示重载请求所基于的版本已过期。
type StaleReloadError struct {
	Expected int64
	Current  int64
}

func (e *StaleReloadError) Error() string {
	return fmt.Sprintf("config: stale reload request: expected revision %d, current is %d", e.Expected, e.Current)
}

// DuplicateReloadError 表示同一请求 ID 的重载被重复提交。
type DuplicateReloadError struct {
	RequestID string
}

func (e *DuplicateReloadError) Error() string {
	return fmt.Sprintf("config: duplicate reload request id %q", e.RequestID)
}

// UnknownPreviewError 表示正式发布引用了不存在（或已被淘汰）的预演。
type UnknownPreviewError struct {
	PreviewID string
}

func (e *UnknownPreviewError) Error() string {
	return fmt.Sprintf("config: unknown preview id %q", e.PreviewID)
}

// StalePreviewError 表示预演所基于的版本已不再是当前版本：
// 预演与发布之间有别的新版本生效，必须重新预演。
type StalePreviewError struct {
	BaseRevision int64
	Current      int64
}

func (e *StalePreviewError) Error() string {
	return fmt.Sprintf("config: stale preview: based on revision %d, current is %d; preview again before publishing",
		e.BaseRevision, e.Current)
}

// PreviewMismatchError 表示预演与正式发布之间来源内容发生了变化，
// 预演结论不能套用到变化后的内容上，发布被拒绝。
type PreviewMismatchError struct {
	PreviewID string
}

func (e *PreviewMismatchError) Error() string {
	return fmt.Sprintf("config: preview %q no longer matches current source contents; preview again before publishing",
		e.PreviewID)
}

// PreviewRejectedError 表示试图正式发布一份预演结论为"不接受"的候选。
type PreviewRejectedError struct {
	PreviewID  string
	Rejections []Rejection
}

func (e *PreviewRejectedError) Error() string {
	return fmt.Sprintf("config: preview %q was rejected (%d rejection(s)); cannot publish a rejected candidate",
		e.PreviewID, len(e.Rejections))
}

// UnknownRevisionError 表示回滚目标版本从未存在（或大于当前版本）。
type UnknownRevisionError struct {
	Revision int64
}

func (e *UnknownRevisionError) Error() string {
	return fmt.Sprintf("config: unknown target revision %d", e.Revision)
}

// EvictedRevisionError 表示回滚目标版本曾存在、但已因超出保留上限被淘汰。
type EvictedRevisionError struct {
	Revision       int64
	OldestRetained int64
}

func (e *EvictedRevisionError) Error() string {
	return fmt.Sprintf("config: target revision %d has been evicted from history (oldest retained is %d)",
		e.Revision, e.OldestRetained)
}
