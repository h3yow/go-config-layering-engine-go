# Config Layering Engine

Go 实现的配置分层加载、校验与热重载库。默认值、配置文件、环境变量、运行期覆盖四类来源
按**固定优先级**合并；类型不匹配、必填缺失、取值非法时**拒绝整份配置并给出可机读的原因**，
绝不静默回退默认值；重载**原子生效**，失败保留上一份可用配置；结构演进通过**声明式迁移规则**
确定性处理改名、移动与删除。发布链路支持**预演 → 发布 → 回滚**：发布前预演候选并逐路径
对比差异，发布时校验预演指纹防止内容漂移，每次生效留痕可回滚，历史保留上限可控。

## 快速开始

```go
schema, _ := config.NewSchema("app", []config.Field{
    {Path: "app.name", Type: config.TypeString, Required: true},
    {Path: "server.port", Type: config.TypeInt, Default: 8080,
        Env: "APP_PORT", Min: ptr(1.0), Max: ptr(65535.0)},
    {Path: "log.level", Type: config.TypeString, Default: "info",
        Enum: []any{"debug", "info", "warn", "error"}},
})

rules := []config.Rule{
    {Kind: config.RuleRename, From: "port", To: "server.port", Since: "v2"},
    {Kind: config.RuleDrop,   From: "debug",              Since: "v2",
        Reason: "use log.level=debug instead"},
}

eng, _ := config.NewEngine(schema, rules)
mgr := config.NewManager(eng,
    config.NewStaticSource("defaults", config.RawConfig{ /* ... */ }), // 最低
    config.NewFileSource("file", "/etc/app/config.json"),
    config.NewEnvSource("env", schema, nil),
    config.NewOverrideSource("override", nil),                        // 最高
)

snap, err := mgr.LoadInitial(ctx)
if err != nil {
    return err // *ResolutionError，Kind 可机读区分原因
}
snap.Values["server.port"]                 // int64(8080)，规范化类型
snap.Sources["server.port"]                // "env" / "file" / ...，生效来源
snap.Warnings                              // 弃用提示（rename/move/drop）

// 热重载：必须带上所基于的版本号，防止过期请求覆盖新配置
next, err := mgr.Reload(ctx, config.ReloadRequest{
    ExpectedRevision: snap.Revision,
    RequestID:        "deploy-2026-09-26-001",
})
mgr.Current() // atomic.Pointer 读取，永不阻塞，永不为半更新状态
```

## 来源优先级（稳定且可解释）

来源按传入 `NewManager` 的顺序从低到高排列，推荐固定为：

| 优先级（低→高） | 来源                | 构造方式                         | 典型内容          |
|-----------------|---------------------|----------------------------------|-------------------|
| 1               | `defaults`         | `NewStaticSource`               | schema 默认值     |
| 2               | `file`             | `NewFileSource`（JSON）          | 部署环境配置      |
| 3               | `env`              | `NewEnvSource`（按 `Field.Env`） | 容器/CI 注入      |
| 4               | `override`         | `NewOverrideSource`              | 运行期 API 覆盖   |

合并规则：

- **对象递归合并**：`server` 这样的中间对象跨层合并，不存在整对象被标量替换；
- **标量/list/null 为叶子，高优先级层整值覆盖低优先级层**；
- 同一叶子路径上**对象与非对象冲突**报 `type_mismatch`，任一方都不会被静默丢弃；
- 结果中的 `Resolved.Sources` 记录每个点分叶子路径的生效来源名，
  `Resolved.Values` 给出规范化后的值，可直接用于解释"为什么是这个值"。

## 拒绝原因（`ResolutionError.Kind`，可区分、可断言）

| Kind                   | 触发场景                                                           |
|------------------------|--------------------------------------------------------------------|
| `source_load`         | 来源读取/解析失败（文件缺失、JSON 非法、环境变量值无法转换）       |
| `type_mismatch`       | 叶子类型与 schema 不符、对象/标量冲突、null 落到非空字段           |
| `missing_required`    | 必填字段在**所有**来源合并后仍缺失                                 |
| `invalid_value`       | 越界（`Min/Max`）、不在 `Enum` 内、列表长度非法                    |
| `unknown_field`       | 出现 schema 未声明且无迁移规则覆盖的字段（历史字段不会被静默丢弃） |
| `migration_conflict`  | 新旧路径同时存在等无法安全迁移的情形                               |

用 `config.AsResolutionError(err)` 取出错误，其 `Path`、`Source`、`Message` 分别说明
**位置、来源层、判定依据**。注意：当高优先级层给出**非法**值时，系统拒绝整份解析
（由 `Manager` 保留旧版本），而不是退回默认值——"非法"和"未设置"严格区分。

## 兼容与迁移规则

规则在 `NewEngine(schema, rules)` 中声明，**按声明顺序**逐来源执行，无 map 遍历，
因此规则不变时同一历史输入的解析结论（含警告顺序）完全可复现。

| Kind     | 语义                                                                 | 警告码                |
|----------|----------------------------------------------------------------------|-----------------------|
| `rename` | `From` 叶子迁移到 `To`，字段改名                                     | `deprecated_rename`   |
| `move`   | 同 rename，语义为"移动分组"（如 `logging.level` → `log.level`）      | `deprecated_move`     |
| `drop`   | 历史字段已删除；出现即移除并提示，不再参与合并                       | `deprecated_drop`     |

安全约束：

1. 规则声明自洽性在构造期校验：`From` 不得重复、rename/move 必须有 `To`、`To` 不得重复；
2. **新旧路径同时出现** → `migration_conflict`（无法判断以谁为准，拒绝，不丢字段）；
3. 迁移后仍出现**未知字段** → `unknown_field`，拒绝；要删除历史字段必须显式声明 `drop`；
4. 每次命中规则都会产生 `Warning{Code, Path, Message}`，附带来源名与 `Since` 版本说明，
   通过 `Resolved.Warnings` 对外可见，可接入启动日志/指标。

## 热重载语义

- **原子生效**：重载先在互斥区内加载全部来源并跑完整解析流水线，全部成功后才通过
  `atomic.Pointer` 替换快照；读取方永不持锁，不可能观察到半更新状态。
- **失败保留**：任一步失败返回错误（消息含 `previous config retained` 与具体来源），
  `Current()` 仍是上一份完整可用配置，版本号不前进。
- **过期拒绝**：`ReloadRequest.ExpectedRevision` 必须等于当前版本，否则返回
  `*StaleReloadError{Expected, Current}`。
- **去重**：`RequestID` 非空时作为幂等键，重复提交返回 `*DuplicateReloadError`
  （无论上一次成功还是失败，客户端须以新 ID 发起修复后的重试）。
- 首次加载用 `LoadInitial`（成功后 revision = 1），未初始化时 `Current()` 返回 `nil`。

## 配置发布：预演 → 发布 → 可回滚

在热重载之上提供"先预演、再发布、可回滚"的发布机制，换配置不再是黑盒。

### 预演（`Preview`）

```go
p, err := mgr.Preview(ctx)   // 不影响当前生效配置
if err != nil { return err } // 仅未初始化等结构性问题
if !p.Acceptable {
    for _, r := range p.Rejections {
        // r.Kind / r.Path / r.Source / r.Message：哪个字段、哪层来源、为什么不接受
    }
    return fmt.Errorf("candidate rejected: %v", p.Rejections)
}
for _, c := range p.Diff {
    // c.Kind: added / changed / removed
    // c.Path, c.OldValue@c.OldSource -> c.NewValue@c.NewSource
}
```

- 预演在互斥区内完整跑一遍"加载 → 迁移 → 合并 → 校验"流水线，但**不触碰当前快照**；
- 候选与当前生效配置逐叶子路径对比，`Diff` 给出每条新增/变更/删除及两侧各自的生效来源；
- 配置问题（非法取值、缺必填、类型不符、未知字段、来源加载失败）**不以 error 返回**，
  而是记入 `Rejections` 并置 `Acceptable=false`——与正常加载同样严格，绝不静默兜底；
- `Preview.Fingerprint` 唯一标识预演时读到的来源内容，`BaseRevision` 记录预演基线版本。

### 发布（`Publish`）

```go
snap, err := mgr.Publish(ctx, config.PublishRequest{
    ExpectedRevision: p.BaseRevision,
    RequestID:        "deploy-2026-10-07-001",
    Preview:          p, // 必须携带预演结论
})
```

发布时重新加载来源并核对，**预演结论对不上就拒绝并保留当前配置**：

| 拒绝原因                                   | 错误类型                |
|--------------------------------------------|-------------------------|
| 未携带预演 / 预演本身不可接受              | `*PreviewMismatchError` |
| 预演后当前版本已前进（基线落后）           | `*PreviewMismatchError` |
| 预演后来源内容变化（指纹不一致）           | `*PreviewMismatchError` |
| `ExpectedRevision` 过期                    | `*StaleReloadError`     |
| `RequestID` 重复                           | `*DuplicateReloadError` |

即"预演通过"和"实际发布的内容"被强制是同一份；两次之间来源变了能发现并拒绝。
发布成功原子生效，历史记录一条 `VersionPublish`。

### 回滚（`Rollback`）与版本历史

```go
hist := mgr.History(10) // 最近 10 版，从新到旧；每条含 Revision/Kind/PublishedAt/Snapshot
snap, err := mgr.Rollback(ctx, config.RollbackRequest{
    ExpectedRevision: mgr.Current().Revision,
    TargetRevision:   hist[3].Revision,
    RequestID:        "rollback-2026-10-07-001",
})
```

- 每次成功生效（初始加载/重载/发布/回滚）都留一条 `VersionRecord`，`Kind` 区分来源；
- 回滚**不重新读取来源**，直接复用历史记录中不可变的快照内容，原子切换后产生**新版本号**
  （内容等同目标版本），`Sources` 溯源完整保留，历史记录一条 `VersionRollback`；
- 回滚在互斥区内完成，读方走原子指针，不可能看到半更新状态；
- 目标版本就是当前版本 / 从未存在 / 已被淘汰 → `*VersionNotFoundError`
  （`Reason` 分别为 `current` / `unknown` / `evicted`）；过期与重复请求同发布一样被拒绝；
  任何拒绝都保留当前正在用的配置。

### 历史保留上限

- 保留数量有上限：默认 `DefaultHistoryLimit`（16），`mgr.SetHistoryLimit(n)` 可随时调整
  （小于 1 按 1 处理），超出后**淘汰最旧**，当前生效版本始终保留；
- 快照不可变：正在被读取或已持有引用的版本不会因淘汰而读坏，只是被淘汰的版本号
  **不能再作为回滚目标**（`evicted`）；
- 请求 ID 去重集同样有界（FIFO，上限 1024），持续高频发布/回滚下内存占用稳定、有界。

## 类型与校验

- JSON 数字按是否为整数确定性解析为 `int64` / `float64`（`json.Decoder.UseNumber`）；
- 环境变量按 `Field.Env` 映射：int/float 做数值解析，bool 接受
  `true/1/yes/on`、`false/0/no/off`，列表按逗号分隔后逐元素转换；
- 叶子规范化后的 Go 类型：`string` / `int64` / `float64` / `bool` / `[]any`；
- `Min/Max` 作用于 int/float 数值或列表长度；`Enum` 作用于标量及列表每个元素。

## 验证方法

```bash
# 全量单测，含并发重载与竞态检测
go test -race -count=1 ./...

# 多次运行确认并发与确定性结论稳定
go test -race -count=10 ./...

# 查看每个用例打印的"输入 / 生效来源 / 判定依据"
go test -race -v -run 'TestResolveSourcePriorityConflict|TestMigration|TestManager' ./...

# 查看预演/发布/回滚用例打印的"候选输入 / 差异结论 / 拒绝原因 / 版本历史"
go test -race -v -run 'TestPreview|TestPublish|TestRollback|TestHistory|TestConcurrent|TestHighFrequency' ./...

# 覆盖率
go test -cover ./...
```

测试覆盖：来源冲突（标量覆盖 + 对象合并 + 来源溯源）、非法取值/类型不匹配/必填缺失/
枚举非法的原因区分、对象-标量冲突、改名/移动/删除迁移、迁移冲突与未知字段拒绝、
迁移可复现性、24 路并发重载（恰好 1 成功、其余 stale）配合持续读取校验无半更新、
重复请求 ID、来源加载失败回滚、env/file 来源端到端；
预演成功（逐条差异 + 来源溯源 + 默认值完整）、预演失败（非法值/缺必填/未知字段定位到
字段与来源层）、发布成功与预演一致、预演后来源漂移（指纹不匹配）拒绝、预演基线过期拒绝、
无预演/不可接受预演拒绝、回滚还原内容与来源溯源、回滚目标已淘汰/不存在/即当前版本拒绝、
回滚过期与重复请求拒绝、历史保留上限与淘汰边界、并发预演/发布/回滚 + 持续读取无半更新、
2000 轮高频发布回滚下历史与去重集有界。`-race` 下无数据竞争报告。

## 兼容范围

预演/发布/回滚与既有能力完全正交、可组合：

- **来源优先级**：预演与发布走同一条合并流水线，`Diff` 与 `Sources` 中的来源名
  就是 `NewManager` 的来源顺序语义；
- **校验**：候选配置与正常加载同样严格——默认值照常注入，非法取值/缺必填/类型不符/
  未知字段照常拒绝，只是结论以 `Preview.Rejections` 呈现而非 error；
- **迁移**：迁移规则在预演流水线中照常执行，`Preview.Warnings` 给出弃用提示；
- **热重载**：`Reload` 与 `Publish`/`Rollback` 共用同一把互斥锁与同一套版本号，
  互为过期检查的对端；`Reload` 成功的版本同样进入历史、可作为回滚目标。

## 文件结构

| 文件             | 职责                                                   |
|------------------|--------------------------------------------------------|
| `errors.go`      | `ErrorKind`、`ResolutionError`、stale/duplicate/预演不匹配/版本不存在错误 |
| `raw.go`         | 配置树类型、点分路径读写、摊平、深拷贝                 |
| `schema.go`      | 字段声明、类型强转、范围/枚举约束、默认层生成          |
| `migration.go`   | rename/move/drop 规则、冲突检测、弃用警告、确定性保证  |
| `source.go`      | Static / Override / JSON File / Env 四类来源           |
| `merge.go`       | 递归分层合并、叶子来源溯源、对象/标量冲突检测          |
| `engine.go`      | 迁移 → 合并 → 形状/叶子/必填校验 的确定性流水线        |
| `manager.go`     | 原子快照、串行写、版本乐观锁、有界请求去重、历史记录与淘汰 |
| `diff.go`        | 两份解析结果的逐路径差异（新增/变更/删除 + 双侧来源）  |
| `release.go`     | 预演、指纹一致性发布、原子回滚、版本历史查询与保留上限 |
