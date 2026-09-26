package config

import "fmt"

// Reason 描述配置被拒绝或告警的类别，便于调用方区分处理。
type Reason string

const (
	ReasonTypeMismatch     Reason = "type_mismatch"     // 来源值与 Schema 声明类型不匹配
	ReasonMissingRequired  Reason = "missing_required"  // 必填字段在所有来源中均缺失
	ReasonInvalidValue     Reason = "invalid_value"     // 取值违反约束（枚举/范围/长度/正则）
	ReasonMigrationFailure Reason = "migration_failure" // 历史配置无法按兼容规则安全迁移
	ReasonStaleReload      Reason = "stale_reload"      // 过期或重复的重载请求
	ReasonSourceError      Reason = "source_error"      // 来源读取失败
	ReasonInternal         Reason = "internal"          // 引擎内部错误
)

// Error 是可区分的配置错误，携带原因、字段路径与来源信息。
type Error struct {
	Reason Reason
	Path   string
	Source string
	Msg    string
}

func (e *Error) Error() string {
	loc := e.Path
	if loc == "" {
		loc = "-"
	}
	src := e.Source
	if src == "" {
		src = "-"
	}
	return fmt.Sprintf("config error [%s] path=%s source=%s: %s", e.Reason, loc, src, e.Msg)
}

// ReasonOf 返回错误原因；非 *Error 时返回 ReasonInternal。
func ReasonOf(err error) Reason {
	if e, ok := err.(*Error); ok {
		return e.Reason
	}
	return ReasonInternal
}

func errf(reason Reason, path, source, format string, args ...any) *Error {
	return &Error{Reason: reason, Path: path, Source: source, Msg: fmt.Sprintf(format, args...)}
}
