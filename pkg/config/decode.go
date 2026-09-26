package config

import (
	"reflect"
	"strconv"
	"strings"
	"time"
)

// decodeInto 将快照中的值按 config tag（缺省取小写字段名）填充到 out。
// 支持：string / int(各种宽度) / uint(各种宽度) / float / bool /
// time.Duration / []string（逗号分隔）以及嵌套结构体（点分路径）。
// 快照中不存在的字段保持零值；类型不匹配返回 ReasonTypeMismatch。
func decodeInto(vals map[string]resolved, out any) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errf(ReasonInternal, "", "", "decode target must be a non-nil pointer, got %T", out)
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return errf(ReasonInternal, "", "", "decode target must point to a struct, got %s", elem.Kind())
	}
	return decodeStruct(vals, "", elem)
}

func decodeStruct(vals map[string]resolved, prefix string, rv reflect.Value) error {
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := strings.ToLower(sf.Name)
		if tag, ok := sf.Tag.Lookup("config"); ok {
			if tag == "-" {
				continue
			}
			name = tag
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Struct {
			ptr := reflect.New(fv.Type().Elem())
			if err := decodeStruct(vals, path, ptr.Elem()); err != nil {
				return err
			}
			fv.Set(ptr)
			continue
		}
		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(time.Duration(0)) {
			if err := decodeStruct(vals, path, fv); err != nil {
				return err
			}
			continue
		}
		r, ok := vals[canonical(path)]
		if !ok {
			continue
		}
		if err := setScalar(fv, path, r); err != nil {
			return err
		}
	}
	return nil
}

func setScalar(fv reflect.Value, path string, r resolved) error {
	raw := r.value
	ft := fv.Type()
	switch {
	case ft == reflect.TypeOf(time.Duration(0)):
		d, err := time.ParseDuration(raw)
		if err != nil {
			return errf(ReasonTypeMismatch, path, r.source, "decode duration %q: %v", raw, err)
		}
		fv.SetInt(int64(d))
		return nil
	case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String:
		parts := strings.Split(raw, ",")
		slice := reflect.MakeSlice(ft, len(parts), len(parts))
		for i, p := range parts {
			slice.Index(i).SetString(p)
		}
		fv.Set(slice)
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return errf(ReasonTypeMismatch, path, r.source, "decode bool %q: %v", raw, err)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, ft.Bits())
		if err != nil {
			return errf(ReasonTypeMismatch, path, r.source, "decode int %q: %v", raw, err)
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, ft.Bits())
		if err != nil {
			return errf(ReasonTypeMismatch, path, r.source, "decode uint %q: %v", raw, err)
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, ft.Bits())
		if err != nil {
			return errf(ReasonTypeMismatch, path, r.source, "decode float %q: %v", raw, err)
		}
		fv.SetFloat(n)
	default:
		return errf(ReasonInternal, path, r.source, "unsupported decode target type %s", ft)
	}
	return nil
}
