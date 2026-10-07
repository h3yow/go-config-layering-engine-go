# Config Layering Engine

Go 实现的配置分层加载、校验、热重载与**预演-发布-回滚**一体化库。schema 默认值、配置文件、
环境变量、运行期覆盖四类来源按**固定优先级**合并；类型不匹配、必填缺失、取值非法时
**拒绝整份配置并给出可机读的原因**，绝不静默回退默认值；重载/发布/回滚**原子生效**，
失败保留上一份可用配置；结构演进通过**声明式迁移规则**确定性处理改名、移动与删除；
每次变更可以先**预演**（候选校验 + 逐路径差异）再**发布**（指纹核对防漂移），
每次成功生效都留**版本历史**，可随时**回滚**到保留中的任一版本。

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
mgr := config.NewManagerWithOptions(eng,
    []config.ManagerOption{config.WithMaxHistory(16)}, // 版本历史保留上限，默认 16
    config.NewFileSource("file", "/etc/app/config.json"),
    config.NewEnvSource("env", schema, nil),
    config.NewOverrideSource("override", nil),                        // 最高
)
// schema 中声明的 Default 会自动作为最低优先级的一层（名为 "defaults"）参与合并，
// 无需再手工提供默认值来源。

snap, err := mgr.LoadInitial(ctx)
if err != nil {
    return err // *ResolutionError，Kind 可机读区分原因
}
snap.Values["server.port"]                 // int64(8080)，规范化类型
snap.Sources["server.port"]                // "env" / "file" / "defaults" / ...，生效来源
snap.Warnings                              // 弃用提示（rename/move/drop）

// 热重载：必须带上所基于的版本号，防止过期请求覆盖新配置
next, err := mgr.Reload(ctx, config.ReloadRequest{
    ExpectedRevision: snap.Revision,
    RequestID:        "deploy-2026-09-26-001",
})
mgr.Current() // atomic.Pointer 读取，永不阻塞，永不为半更新状态
```

## 先预演、再发布、可回滚

`Reload` 是"直接重读来源并生效"。如果希望**改之前先看清会改成什么样、出问题能退回去**，
用预演（Preview）→ 发布（Publish）→ 回滚（Rollback）三步：

```go
// 1) 预演：完整跑一遍候选加载与校验，不影响当前生效配置
pv, err := mgr.Preview(ctx, config.PreviewRequest{
    ExpectedRevision: mgr.Current().Revision,
    RequestID:        "preview-2026-10-07-001",
})
if err != nil { /* 请求级问题：过期(*StaleReloadError)/重复(*DuplicateReloadError) */ }

if !pv.Accepted {
    // 候选不被接受：逐条给出哪个字段、哪层来源、为什么
    for _, r := range pv.Rejections {
        log.Printf("rejected: kind=%s path=%s source=%s reason=%s",
            r.Kind, r.Path, r.Source, r.Message)
    }
    return
}
// 候选与当前生效配置的逐路径差异：新增/变更/删除，各自来自哪一层
for _, d := range pv.Diff {
    log.Printf("%s %s: %v(%s) -> %v(%s)", d.Kind, d.Path, d.OldValue, d.OldSource, d.NewValue, d.NewSource)
}

// 2) 正式发布：重新读取来源并核对指纹；内容在预演后变过就拒绝
snap, err = mgr.Publish(ctx, config.PublishRequest{
    PreviewID:        pv.ID,
    ExpectedRevision: pv.BaseRevision,
    RequestID:        "publish-2026-10-07-001",
})

// 3) 需要退回时：按版本号回滚（回滚本身也产生一个新版本）
snap, err = mgr.Rollback(ctx, config.RollbackRequest{
    TargetRevision:   1, // 必须是仍保留在历史中的版本
    ExpectedRevision: mgr.Current().Revision,
    RequestID:        "rollback-2026-10-07-001",
})

// 查看最近历史（从新到旧）
for _, rec := range mgr.History(5) {
    log.Printf("rev=%d kind=%s at=%s note=%s", rec.Revision, rec.Kind, rec.PublishedAt, rec.Note)
}
```

### 预演语义

- 预演在与写路径相同的互斥区内**完整加载全部来源并跑完整解析流水线**（迁移 → 合并 →
  形状/叶子/必填校验），但**不触碰当前生效快照**；并发读方完全无感。
- 候选**可接受**时：`Preview.Accepted=true`，`Candidate` 是完整解析结果（含 schema 默认值、
  每个叶子路径的生效来源），`Diff` 是与当前生效快照的逐路径差异
  （`added`/`changed`/`removed`，每条带两侧来源名）。
- 候选**不可接受**时：`Accepted=false`，`Rejections` 逐条给出 `Kind`（与
  `ResolutionError.Kind` 一致）、`Path`、`Source`、`Message`——非法取值、缺必填、
  类型不符、未知字段、迁移冲突都会在这里显形，**不会拿默认值兜底**。
- 请求级问题（未初始化、`ExpectedRevision` 过期、`RequestID` 重复）返回 Go 错误，
  语义与 `Reload` 一致。

### 预演与发布的对账（防漂移）

- 预演时对加载到的全部来源内容计算**指纹**（来源名 + 内容的 SHA-256，键序确定）。
- `Publish` 会**重新读取全部来源**并重算指纹：与预演不一致即返回
  `*PreviewMismatchError`，当前配置保持不动——"预演通过"绝不会被套用到变化后的内容上。
- 指纹一致时解析流水线是确定性的，发布内容就是预演结论，原子生效。
- 预演与发布之间若有**别的版本生效**（Reload/Publish/Rollback），发布会得到
  `*StalePreviewError{BaseRevision, Current}`，必须重新预演。
- 引用不存在（或已被淘汰）的预演得到 `*UnknownPreviewError`；发布"预演未通过"的候选
  得到 `*PreviewRejectedError`。待发布预演数量有上限（32），超出淘汰最早的。

### 版本历史与回滚

- 每次成功生效（`LoadInitial`/`Reload`/`Publish`/`Rollback`）都追加一条
  `VersionRecord{Revision, Kind, PublishedAt, Note, Snapshot}`。
- `History(limit)` 按版本号**从新到旧**返回保留中的记录；`limit<=0` 返回全部。
- **保留上限**：`WithMaxHistory(n)`（默认 16，最小 1），超出后淘汰最旧记录。
  被淘汰的只是历史列表中的位置——快照本身不可变，已持有它的读者永远读到完整数据；
  但**不能再回滚到已淘汰的版本**（得到 `*EvictedRevisionError{Revision, OldestRetained}`）。
- `Rollback` 把当前配置**原子**切回目标版本的内容（含每个值的生效来源解释），
  回滚本身产生一个递增的新版本并记入历史（`Kind=rollback`），因此回滚后还能再回滚。
- 目标版本从未存在得到 `*UnknownRevisionError`；`ExpectedRevision` 过期得到
  `*StaleReloadError`；`RequestID` 重复得到 `*DuplicateReloadError`。
  任何失败都**保留当前生效配置**。
- 内存有界：历史 ≤ `MaxHistory` 条、待发布预演 ≤ 32 条、请求去重集合 ≤ 4096 条
  （FIFO 淘汰最旧），持续高频发布/回滚下内存占用稳定。

## 来源优先级（稳定且可解释）

来源按传入 `NewManager` 的顺序从低到高排列；schema 默认值自动作为最低的
`defaults` 层前置，推荐整体固定为：

| 优先级（低→高） | 来源                | 构造方式                         | 典型内容          |
|-----------------|---------------------|----------------------------------|-------------------|
| 0               | `defaults`（自动） | schema `Field.Default`           | 声明内默认值      |
| 1               | `file`             | `NewFileSource`（JSON）          | 部署环境配置      |
| 2               | `env`              | `NewEnvSource`（按 `Field.Env`） | 容器/CI 注入      |
| 3               | `override`         | `NewOverrideSource`              | 运行期 API 覆盖   |

（仍可以手工再传一个 `defaults` 来源，它会覆盖自动默认层，行为与之前版本兼容。）

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

- **原子生效**：重载/发布/回滚先在互斥区内加载全部来源并跑完整解析流水线，全部成功后才通过
  `atomic.Pointer` 替换快照；读取方永不持锁，不可能观察到半更新状态。
- **失败保留**：任一步失败返回错误（消息含 `previous config retained` 与具体来源），
  `Current()` 仍是上一份完整可用配置，版本号不前进。
- **过期拒绝**：`ReloadRequest.ExpectedRevision` 必须等于当前版本，否则返回
  `*StaleReloadError{Expected, Current}`。
- **去重**：`RequestID` 非空时作为幂等键，重复提交返回 `*DuplicateReloadError`
  （无论上一次成功还是失败，客户端须以新 ID 发起修复后的重试）。
- 首次加载用 `LoadInitial`（成功后 revision = 1），未初始化时 `Current()` 返回 `nil`。

## 与预演/回滚的兼容范围

- 预演与回滚走**同一条解析流水线**：迁移规则、来源优先级、类型/枚举/范围/必填校验、
  schema 默认值装载完全一致——预演看到的就是发布会生效的，回滚恢复的就是当时生效的。
- 预演的差异对比基于**规范化后的叶子值与生效来源**，与 `Resolved.Values/Sources` 同源。
- 历史快照记录的是解析后的不可变结果；回滚不重新读取来源、不重新跑迁移，
  因此回滚结果与当初生效时**逐位一致**。
- 迁移警告（`Warnings`）同样随快照保存，回滚后可原样取回。

## 类型与校验

- JSON 数字按是否为整数确定性解析为 `int64` / `float64`（`json.Decoder.UseNumber`）；
- 环境变量按 `Field.Env` 映射：int/float 做数值解析，bool 接受
  `true/1/yes/on`、`false/0/no/off`，列表按逗号分隔后逐元素转换；
- 叶子规范化后的 Go 类型：`string` / `int64` / `float64` / `bool` / `[]any`；
- `Min/Max` 作用于 int/float 数值或列表长度；`Enum` 作用于标量及列表每个元素。

## 验证方法

```bash
# 全量单测，含并发重载/发布/回滚与竞态检测
go test -race -count=1 ./...

# 多次运行确认并发与确定性结论稳定
go test -race -count=10 ./...

# 查看每个用例打印的"输入 / 生效来源 / 差异结论 / 判定依据"
go test -race -v -run 'TestResolve|TestMigration|TestManager|TestPreview|TestPublish|TestRollback|TestHistory' ./...

# 覆盖率
go test -cover ./...
```

测试覆盖：来源冲突（标量覆盖 + 对象合并 + 来源溯源）、非法取值/类型不匹配/必填缺失/
枚举非法的原因区分、对象-标量冲突、改名/移动/删除迁移、迁移冲突与未知字段拒绝、
迁移可复现性、24 路并发重载（恰好 1 成功、其余 stale）配合持续读取校验无半更新、
重复请求 ID、来源加载失败回滚、env/file 来源端到端；
预演成功（新增/变更/删除差异与来源）、预演失败（非法值/类型不符逐条定位）、
预演与发布之间来源漂移被指纹核对拒绝、未知/过期/被拒绝预演的发布拒绝、
过期与重复的预演/发布/回滚请求、并发预演+发布+回滚+读取无半更新、
回滚恢复内容与来源解释、历史淘汰边界（已淘汰版本拒绝回滚、保留中最旧版本可回滚）、
被淘汰快照对既有读者仍完整可读、2000 轮高频发布/回滚下历史/预演/去重集合均有界。
`-race` 下无数据竞争报告。

## 文件结构

| 文件             | 职责                                                   |
|------------------|--------------------------------------------------------|
| `errors.go`      | `ErrorKind`、`ResolutionError`、stale/duplicate/预演与回滚错误 |
| `raw.go`         | 配置树类型、点分路径读写、摊平、深拷贝                 |
| `schema.go`      | 字段声明、类型强转、范围/枚举约束、默认层生成          |
| `migration.go`   | rename/move/drop 规则、冲突检测、弃用警告、确定性保证  |
| `source.go`      | Static / Override / JSON File / Env 四类来源           |
| `merge.go`       | 递归分层合并、叶子来源溯源、对象/标量冲突检测          |
| `engine.go`      | 迁移 → 合并 → 形状/叶子/必填校验 的确定性流水线        |
| `manager.go`     | 原子快照、串行写路径、版本乐观锁、请求去重、历史登记   |
| `release.go`     | 发布前预演、逐路径差异、来源指纹、正式发布             |
| `history.go`     | 版本历史（有界保留）、按版本回滚                       |
