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
