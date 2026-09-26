package config

import "context"

// loadAll 读取所有来源并返回按优先级降序排列的数据（稳定排序）。
// 任一来源读取失败即返回带来源名的错误，不产生部分结果。
func loadAll(ctx context.Context, sources []Source) ([]namedData, error) {
	ordered := sortSourcesDesc(sources)
	loaded := make([]namedData, 0, len(ordered))
	for _, src := range ordered {
		data, err := src.Load(ctx)
		if err != nil {
			return nil, wrapSourceError(src.Name(), err)
		}
		norm := make(map[string]string, len(data))
		for k, v := range data {
			norm[canonical(k)] = v
		}
		loaded = append(loaded, namedData{name: src.Name(), data: norm})
	}
	return loaded, nil
}

// build 执行一次完整的 加载->迁移->合并->校验，产出新快照。
// 任何阶段失败都返回错误，调用方负责保留旧快照。
func build(ctx context.Context, schema *Schema, sources []Source, version uint64) (*snapshot, error) {
	loaded, err := loadAll(ctx, sources)
	if err != nil {
		return nil, err
	}

	// 逐来源应用兼容规则（优先级降序，保证告警顺序可复现）
	var migrationWarns []Warning
	migrated := make([]namedData, 0, len(loaded))
	for _, nd := range loaded {
		md, warns, merr := applyRules(schema, nd.name, nd.data)
		if merr != nil {
			return nil, merr
		}
		migrationWarns = append(migrationWarns, warns...)
		migrated = append(migrated, namedData{name: nd.name, data: md})
	}

	merged := merge(migrated)

	finalVals, fieldWarns, verr := validateAndFill(schema, merged)
	if verr != nil {
		return nil, verr
	}

	warns := make([]Warning, 0, len(migrationWarns)+len(fieldWarns))
	warns = append(warns, migrationWarns...)
	warns = append(warns, fieldWarns...)

	return &snapshot{version: version, vals: finalVals, warns: warns}, nil
}

func wrapSourceError(name string, err error) error {
	if e, ok := err.(*Error); ok {
		if e.Source == "" {
			e.Source = name
		}
		return e
	}
	return errf(ReasonSourceError, "", name, "%v", err)
}
