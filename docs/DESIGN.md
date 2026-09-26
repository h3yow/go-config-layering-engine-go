# 设计说明

## 1. 来源与优先级

引擎内置四个层级，优先级**从高到低**固定如下（数值越大越优先）：

| 优先级常量              | 值  | 来源     | 说明                                   |
| ----------------------- | --- | -------- | -------------------------------------- |
| `PriorityOverride`      | 300 | override | 运行期覆盖（内存 MapSource），最高优先 |
| `PriorityEnv`           | 200 | env      | 环境变量                               |
| `PriorityFile`          | 100 | file     | JSON 配置文件                          |
| `PriorityDefaults`      | 0   | defaults | 代码内默认值（内存 MapSource）         |

此外，`Field.Default` 声明的字段级默认值是**最后兜底**：仅当所有来源都未提供
该键时以虚拟来源 `default` 填入，因此任何真实来源的非法值都不会被默认值掩盖。

合并规则（确定性）：

1. 所有来源数据扁平化到点分路径（`server.port`），键名统一小写规范化；
2. 按优先级降序稳定排序（同优先级按来源注册顺序）；
3. **高优先级先占位，低优先级只补缺**。同键冲突时高优先级胜，无二次回退；
4. 合并结果逐字段校验：

   - 类型不匹配（如 `server.port=abc`）→ `type_mismatch`；
   - 取值非法（枚举外、越 Min/Max、正则不匹配）→ `invalid_value`；
   - 必填且所有来源缺失、无默认值 → `missing_required`；
   - 出现未声明且无迁移规则命中的字段 → `invalid_value`（拒绝静默丢字段）；
   - 读取来源本身失败（文件不存在、JSON 非法）→ `source_error`。

   **任何字段非法都会让整份配置被拒绝，绝不静默回退默认值。**
5. `Manager.Explain(path)` 返回 `(value, source, ok)`，解释字段最终值由谁决定。

支持的字段类型：`string`、`int`、`float`、`bool`、`duration`
（`time.ParseDuration`）、`strings`（字符串切片，文件中为标量数组/逗号串）。

来源键映射：

- **文件**：嵌套 JSON 对象展开为点分路径；`{"server":{"port":9000}}` →
  `server.port=9000`；标量数组 `["a","b"]` → `a,b`。
- **环境变量**：前缀过滤后 `APP_SERVER_PORT` → `server.port`
  （大写转小写、`_` 转 `.`）。
- **内存来源**：直接使用点分键，常用于默认值与运行期覆盖。

## 2. 兼容与迁移规则

规则在 Schema 上**按声明顺序**应用于每个来源（高优先级来源先处理，
告警顺序因此可复现）。规则集不变时，同一历史输入的解析结论完全可复现。

### 改名 / 移动：`Rename(from, to)`

| 历史来源情况                       | 判定                                                         |
| ---------------------------------- | ------------------------------------------------------------ |
| 只有旧键 `from`                    | 值迁移到 `to`，产生弃用提示                                  |
| 旧键与新键都在，且**值相同**       | 丢弃旧键，产生弃用提示（幂等迁移）                           |
| 旧键与新键都在，但**值不同**       | `migration_failure`：无法安全迁移，拒绝，不静默选边          |

迁移后的值仍须通过目标字段的类型与约束校验。

### 删除：`Delete(from, policy)`

- `DeleteReject`（默认）：历史配置仍携带该字段 → `migration_failure`，
  **拒绝而非静默丢字段**，强制调用方更新配置；
- `DeleteDropWarn`：显式确认可丢弃 → 删除该值并产生弃用提示。

### 弃用提示

- 迁移产生的提示（改名、丢弃）记录在快照的 `Warnings()` 中；
- `Field.Deprecated` 非空时，任何来源为该字段赋值也会产生弃用提示；
- 提示为非致命信息，不改变取值；拒绝原因则以 `*config.Error` 返回，
  可用 `config.ReasonOf(err)` 区分：
  `type_mismatch` / `missing_required` / `invalid_value` /
  `migration_failure` / `stale_reload` / `source_error`。

## 3. 热重载与并发模型

- 快照（`snapshot`）以 `atomic.Pointer` 持有：**读路径完全无锁**，
  每次 `Get/Explain/Decode/Warnings` 看到的都是一份完整快照；
- 需要**跨多个键的一致视图**时，使用 `Snapshot()` 获取不可变 `View`：
  一次原子加载后，视图内所有字段属于同一版本，即使随后发生重载，
  视图内容也不会变化；注意连续两次 `Manager.Get` 可能分属不同版本，
  成对/成组读取务必通过同一个 `View` 进行；
- `Reload` 由互斥锁串行化，期间构建全新快照，成功后一次性原子替换；
  并发调用方不可能观察到"部分键已更新、部分仍旧"的中间态；
- 重载失败时新快照被丢弃，**上一份可用配置继续生效**，版本号不变，
  错误原因可区分；
- 乐观并发控制：`Reload(ctx, expectVersion)`：
  - `expectVersion == 0`：无条件重载；
  - 非 0 且不等于当前版本 → `stale_reload`，不产生任何变更
    （覆盖"重复提交"与"过期请求"两种情况）；
  - 并发基于同一版本提交时，至多一个成功，其余全部 `stale_reload`。

## 4. 验证方法

```bash
go vet ./...
go test -race -v ./...
```

测试覆盖矩阵：

| 场景             | 测试                           |
| ---------------- | ------------------------------ |
| 四级来源冲突     | `TestSourcePriorityConflict`   |
| 类型/枚举/范围非法 | `TestInvalidValuesRejected`    |
| 必填缺失、未知字段 | `TestMissingRequired` / `TestUnknownFieldRejected` |
| 文件+环境来源端到端 | `TestFileAndEnvSources`      |
| 并发重载原子性   | `TestConcurrentReloadAtomicity`|
| 过期/重复请求拒绝 | `TestStaleReloadRejected`      |
| 失败保留旧配置   | `TestFailedReloadKeepsPrevious`|
| 并发串行化       | `TestConcurrentReloadsSerialized` |
| 改名/删除/冲突迁移 | `TestRename*` / `TestDeleteRejectPolicy` |
| 结论可复现       | `TestMigrationReproducible`    |
| 迁移后仍校验     | `TestMigratedValueStillValidated` |
| 结构体解码       | `TestDecodeIntoStruct`         |

每个用例在测试日志中打印**输入、生效来源与判定依据**（见 `-v` 输出）。
`-race` 下全部通过，无数据竞争。

## 5. 范围与非目标

- 文件来源仅支持 JSON；环境来源不做类型推断（统一按字符串交给 Schema 校验）；
- 不内置文件 watch / 定时轮询：由调用方决定时机后调用 `Reload`；
- 来源须在 Manager 构造时固定；运行期对 `MapSource.Set` 的调用应在没有并发
  读取的时刻进行（随后通过 `Reload` 原子生效）。
