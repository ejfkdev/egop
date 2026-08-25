# 示例总览

可跑示例都在 `examples/` 下，每个目录自带 README。这是按主题的一览表：

| 目录 | 主题 | 运行方式 |
|---|---|---|
| `inproc` | 进程内全链路（注册/调用/配置/事件） | `go run .` |
| `dependency` | 依赖声明 / 跨插件调用 / 批量装载 / 槽位契约 | `go run .` |
| `lazy` | 懒注册 + 批量拓扑排序（依赖后到自动补载） | `go run .` |
| `slots` | 槽位契约 `Slot`/`SlotSpec`（八轴 + needs） | `go run .` |
| `tools` | 工具面 `ToolFunc`/`ToolRaw` | `go run .` |
| `hooks` | Hook 派发（`Block`/`Reason`/`Data` + 回滚） | `go run .` |
| `events` | 事件总线发布/订阅（过滤 + 标签 + 通配） | `go run .` |
| `origin` | 溯源 `Origin`（事件/调用/hook 同源） | `go run .` |
| `config` | 配置 + 函数 schema 校验（默认开） | `go run .` |
| `capabilities` | 能力面 `Surface` + 扩展 `Op` | `go run .` |
| `net` | 出站网络 `Net`（Request / DialStream + 协议门） | `go run .` |
| `exchange` | 信封翻译表 `Register`/`NewEvent`/`Decode` | `go run .` |
| `fs` | `io/fs.FS` 注入（跨平台读插件字节） | `go run .` |
| `storage` | 插件专属持久化 `Storage` 注入（Persist/KV） | `go run .` |
| `wasm` | 最小 `*.egop.wasm` 插件 | `go test .` |
| `rawconn` | 远程通道裸字节流（传输无关） | `go run .` |
| `hotreload` | 目录热更 Watch（两段确认 + 回退保旧） | `go run .` |
| `collab` | 跨插件元数据 / 配置读写权限 | `go run .` |
| `controlplane` | 宿主控制面（Snapshot/Dependents/CapabilityIndex/Functions/Tools） | `go run .` |

对照文档：`doc/contract.md`（契约与不变量）、`doc/api.md`（API 签名）、
`doc/usage.md`（按主题的最小片段）、`doc/decisions.md`（设计取舍）。