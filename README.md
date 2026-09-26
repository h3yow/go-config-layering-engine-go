# go-config-layering-engine

配置分层加载与热重载引擎：默认值、配置文件、环境变量与运行期覆盖四类来源
确定性合并，带类型/约束校验、结构演进兼容规则，以及原子热重载。

## 快速开始

```go
import "github.com/h3yow/go-config-layering-engine-go/pkg/config"

schema := config.NewSchema().
    MustAdd("server.host", config.Field{Type: config.TypeString, Default: "127.0.0.1"}).
    MustAdd("server.port", config.Field{Type: config.TypeInt, Min: "1", Max: "65535", Default: "8080"}).
    MustAdd("db.dsn",      config.Field{Type: config.TypeString, Required: true}).
    MustAdd("http.timeout", config.Field{Type: config.TypeDuration, Min: "1s", Max: "1m"}).
    AddRule(config.Rename("db.url", "db.dsn")).          // 历史键改名
    AddRule(config.Delete("cache.oldttl", config.DeleteDropWarn))

m, err := config.NewManager(ctx, schema,
    config.NewMapSource("defaults", config.PriorityDefaults, defaults),
    config.NewFileSource("file", config.PriorityFile, "/etc/app/config.json"),
    config.NewEnvSource("env", config.PriorityEnv, "APP"),
    config.NewMapSource("override", config.PriorityOverride, runtimeOverrides),
)

host, _ := m.Get("server.port")                 // 单键读取
_, src, _ := m.Explain("server.port")           // 同时拿到生效来源
view := m.Snapshot()                            // 多键一致视图（不可变、免锁）
h, _ := view.Get("server.host")
p, _ := view.Get("server.port")                 // h 与 p 保证属于同一版本
var cfg AppConfig
_ = m.Decode(&cfg)                              // 反射填充到结构体

v, err := m.Reload(ctx, m.Version())            // 基于版本号的乐观重载
// 版本不匹配 -> config.ReasonOf(err) == config.ReasonStaleReload
```

## 验证方法

```bash
go vet ./...
go test -race -v ./...          # 竞态检测 + 打印输入/生效来源/判定依据
```

详细的优先级、兼容与迁移规则见 [docs/DESIGN.md](docs/DESIGN.md)。
