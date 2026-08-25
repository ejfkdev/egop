# 契约（contract）

本文描述插件与宿主之间的**词汇与线上格式**——`contract` 包是真源。egop 全程
只有一种编码：**JSON**。

## 1. 结果信封（唯一编码）

跨边界（WASM ABI 与远程通道帧）的结果统一为：

```json
{"ok": bool, "result": any, "result_b64": string, "error": string}
```

- `ok=false` 时 `error` 为人类可读消息；
- 二进制值（文件 / KV 等）走 `result_b64`（base64）；
- `result` 为 JSON 载荷（函数返回值、配置等）。

对应 Go 类型 `contract.ResultEnvelope`。

## 2. 插件元数据 Meta

`Meta` 是插件**自述**，分两块：`Provides`（我提供什么）与 `Requires`（我要什么），
外加 `Slot`（声称实现的槽位）。所有字段都有 JSON tag（跨边界同名）。

```go
type Meta struct {
    ID, Name, Version, Description
    Homepage, License string   // 项目主页 / SPDX 许可证(描述性,可选)
    Authors, Tags []string      // 作者 / 关键词(描述性,可选)
    Provides Provides // 供给面
    Requires Requires // 依赖面
    Slot     string   // 声称实现的槽位
    DependsOn []string // 遗留纯声明依赖(展示/排序用,非强制校验轴)
}
```

`Homepage`/`License`/`Authors`/`Tags` 是**描述性元数据**（非契约轴，供目录、控制面展示与检索），
均 `omitempty`，旧清单不带它们也照常解析。

**Provides（我提供什么）**：

| 字段 | 类型 | 语义 |
|---|---|---|
| `points` | `[]string` | 保证发射的框架点位（宿主 EnsurePoint） |
| `hooks` | `[]HookPointSpec` | 对外 hook 点（kind=modify/observe） |
| `events` | `[]EventTopicSpec` | 对外发布的事件主题 |
| `functions` | `[]FuncSpec` | 可调用函数目录 |
| `capabilities` | `[]string` | 能力词（先说后做，Surface 视图按此裁剪） |
| `config` | `[]ConfigFieldSpec` | 可下发配置字段（key + Schema + 跨插件读写标志，见 §6） |

**Requires（我要什么）**：

| 字段 | 类型 | 语义 |
|---|---|---|
| `listens` | `[]string` | 要订阅的框架点位 |
| `deps` | `[]Dependency` | 依赖声明（见 §7） |
| `tools` | `[]string` | 依赖的**工具**名（框架须先就位，注册时机器校验） |

> `points`/`listens` 是**声明式契约轴**：注明「我发射/我订阅哪些固定点位」，注册时
> 由宿主 `EnsurePoint` 记录在案（供槽位求差与装配自检）。运行时的真正投递走**事件
> 主题**——`event.emit`/`event.listen` 能力 + `PublishEvent`/`SubscribeEvent`（见 §6）。

`FuncSpec` 形状：

```go
type FuncSpec struct {
    Name        string          `json:"name"`
    Description string          `json:"description,omitempty"`
    Input       json.RawMessage `json:"input,omitempty"`  // 入参 JSON Schema
    Output      json.RawMessage `json:"output,omitempty"` // 返回 JSON Schema
}
```

## 3. 工具 与 Manifest

"工具"= 带不透明上下文、返回字符串的可调用体。它与函数**共用同一种 `FuncSpec`**
（§2），区别只在**执行形态**（返回字符串 + 多一个 `tctx`）与**注册面**（声明
`tool.provide` 能力 + 列入 `Manifest.Tools`）。

```go
type Manifest struct {
    Meta
    Tools []FuncSpec `json:"tools,omitempty"`
}
```

工具执行签名 `ToolFunc[C] func(ctx, tctx *C, input json.RawMessage) (json.RawMessage, error)`；
loader 侧保持无类型（`ToolRaw(...)(func(ctx, tctxJSON, args) (string, error), bool)`），
typed 包装永远在消费方装配层。

## 4. 槽位契约 SlotSpec（八轴 + Needs）

`SlotSpec` 是"业务模块槽位"的**最小契约**（只列名字），实现声称该槽位时逐轴
求差（只多不少）。字段名与 `Meta` 同轴字段一致：

| 轴 | Meta 字段 | SlotSpec 字段 |
|---|---|---|
| 发射点位 | `provides` | `provides` |
| 自有 hook | `hooks` | `hooks` |
| 事件主题 | `events` | `events` |
| 函数面 | `functions` | `functions` |
| 能力面 | `capabilities` | `capabilities` |
| 配置面 | `config` | `config` |
| 可监点 | `listens` | `listens` |
| 工具依赖 | `needs_tools` | `needs_tools` |

另有 `needs`（前置槽位名清单,与 `Meta.Requires.Deps` 形状不同）、`builtin bool`。

## 5. 能力词（capabilities）

预置能力词常量（内容无关；业务域可在此基础上追加自己的词）：

| 常量 | 词 | 语义 |
|---|---|---|
| `CapCallPlugins` | `plugin.call` | 经宿主调用别的插件函数 |
| `CapPluginMeta` | `plugin.meta` | 读插件目录/其它插件元数据 |
| `CapEmitsEvents` | `event.emit` | 发布事件 |
| `CapListensEvents` | `event.listen` | 订阅事件 |
| `CapPersist` | `storage.persist` | 插件专属文件读写 |
| `CapKV` | `storage.kv` | 插件专属 KV |
| `CapExec` | `exec.cmd` | 执行命令 |
| `CapNet` | `net.access` | 出站网络(HTTP/HTTPS/SSE/WebSocket/WebTransport 等,经 Net 注入) |
| `CapTools` | `tool.provide` | 向工具面提供工具 |
| `CapConfigRead` | `config.read` | 读其它插件的声明配置字段 |
| `CapConfigWrite` | `config.write` | 写其它插件的声明配置字段 |

动态点位/主题 id 前缀：`dyn.`（`PointID`/`EventID` 做命名空间化）。

## 6. Surface 能力面

注册时宿主按 `Meta.Provides.Capabilities` 注入**裁剪后的** `contract.Surface` 视图。方法见
api.md 的 Surface 能力面；每条能力的门控语义见 usage.md 的"ctx 能力面对照"。

### 配置字段读写权限

插件用 `Provides.Config` 声明可下发字段，`ConfigFieldSpec` 带两个**跨插件**访问标志
（`egop`/宿主始终可读写）：

| 标志 | 语义 | 默认 |
|---|---|---|
| `Readable` | 其它插件可读该字段（须声明 `config.read`） | `false` |
| `Writable` | 其它插件可写该字段（须声明 `config.write`） | `false` |

两层同时满足才放行：调用方要有能力词 + 目标字段要标记。典型例子——某 `apikey`
字段 `Writable:true, Readable:false`：egop 可读写，别的插件只能写入、读不回。

跨插件访问经 `Surface.GetConfig(pluginID, key)`（读）/ `Surface.SetConfig(pluginID, key, value)`
（写）；egop/宿主读整份生效配置走 `Host.AppliedConfig`/`Host.GetConfig`。

### 插件目录与元数据

`Surface.Plugins() / GetPlugin(id)` 提供「当前已加载插件列表 + 其它插件
元数据（`Meta`）」，统一受 `plugin.meta` 能力门控——未声明者完全不可见（空/`false`）。

### 持久化注入

`Surface.Persist()` / `KV()`（`storage.persist` / `storage.kv`）的**后端由外部注入**
（与 `io/fs.FS` 读侧、网络 `Stream` 同理）：装配层实现 `contract.Storage` 并从
`host.Options.Storage` **必填注入**，宿主把**原始 pluginID** 转发给实现——命名空间、
目录布局、hash 等存储策略由实现自行决定（egop 不越权、不内置任何文件/网络能力）。
未注入则 `Persist()`/`KV()` 返回不可用。

### 出站网络

`Surface.Net()`（`net.access`）提供插件**出站网络**：装配层实现 `contract.Net` 并从
`host.Options.Net` **必填注入**，宿主持能力门控转发。egop 只定义最小面与数据结构
（`Request`/`Response`/`Stream`），不实现任何传输。单向请求族（HTTP/HTTPS/SSE/gRPC-Web/
JSON-RPC/GraphQL/REST）统一走 `Net.Request`——SSE 即 `Response.Body` 的流式长响应、
gRPC(-Web) unary 的尾部元数据在 `Response.Trailers`；双向消息流族（WebSocket/WebRTC
DataChannel/WebTransport/MQTT-over-WS）统一走 `Net.DialStream`（URL scheme 决定传输，
返回 `Stream` 字节消息流）。桌面装配层用 `net/http`+websocket、浏览器 wasm 用
`fetch`/`WebSocket`/`WebTransport` 实现同一面，egop 自身零网络依赖。

**网络协议门**：宿主在把出站动作转交装配实现前先校验目标必须是**网络协议**——
方案在 `http`/`https`/`ws`/`wss`（及 `Options.NetSchemes` 补充方案）内才放行，
`file://`、`data:`、`javascript:`、裸路径、协议相对 `//host`、空 URL 一律拒绝。
这防止插件借用出站网络后端把 `io.Reader`/`Stream` 接到本地文件系统，与
"先说后做"同属第一道防线。

### 溯源 Origin

事件与跨插件调用共用**同一个**来源结构体 `contract.Origin`：

```go
type Origin struct {
    ID      string     `json:"id,omitempty"`      // 来源插件 id(空 = 宿主/框架)
    Version string     `json:"version,omitempty"` // 来源插件版本
    Kind    OriginKind `json:"kind,omitempty"`    // event / hook / call / host
    Point   string     `json:"point,omitempty"`   // 主题 / 函数名 / 钩子点
    At      int64      `json:"at,omitempty"`      // 来源产生时间戳(ms)
}
```

- **事件**：订阅者经 `Event.Source`（`*Origin`）读来源；框架填 `ID/Version/Kind/Point/At`，
  宿主级事件 `Kind=host`（如 `plugin.config.updated`）。
- **跨插件调用**：被调函数经 `contract.OriginFrom(ctx)` 读调用者（`Kind=call`、
  `Point=被调函数名`）；由宿主/应用直接 `Host.Call` 时为 `Origin{Kind:"host"}`——
  插件始终能拿到一个显式的来源，无需区分「无来源」。

`origin` 通过 `contract.WithOrigin(ctx, o)` 注入、`contract.OriginFrom(ctx)` 读取。

### Hook 派发

`Surface.OnHook(hookID, fn)` 注册 hook 回调（插件卸载/热替换时框架自动回滚）；
应用侧用 `Host.TriggerHook(ctx, hookID, data)` 触发，每个回调返回一个 `HookResult`：

```go
type HookFunc func(ctx context.Context, hookID string, data json.RawMessage) any

type HookResult struct {
    Block  bool            `json:"block,omitempty"`  // 回调写:是否阻断后续
    Reason string          `json:"reason,omitempty"` // 回调写:描述性理由
    Data   json.RawMessage `json:"data,omitempty"`   // 回调写:产出数据(自带上下文)
    Origin *Origin         `json:"origin,omitempty"` // 框架填:来源(hook 触发)
    Seq    int             `json:"seq,omitempty"`    // 框架填:回调顺序(1 起)
}
```

回调返回 `any`：返回 `HookResult` 即带上下文的完整形态；直接返回数据（`nil` /
`json.RawMessage` / `[]byte` / `string` / 数值 / 结构体 / map / 切片）则由框架经
`contract.HookResultOf` 归一成 `HookResult`（`Block=false`、`Data`=该值的 JSON 编码）。
框架在触发时统一回填 `Origin`（`ID/版本/Kind:"hook"/Point/At`）与 `Seq`——"阻断"不靠
返回 nil/false 含糊判定，且有"谁、何时、第几个"的执行上下文（与事件/调用的来源同构）。
宿主注入）注册，触发时框架把 hook 帧 / `egop_on_hook` 回调送进插件，与 Go 侧
`OnHook` 同语义。

## 7. 依赖 Dependency

```go
type Dependency struct {
    Plugin     string         `json:"plugin,omitempty"` // 点名插件
    Slot       string         `json:"slot,omitempty"`   // 点到槽位面
    Kind       DependencyKind `json:"kind"`             // init | call | soft
    MinVersion string         `json:"min_version,omitempty"`
}
```

- `DepInit`：硬依赖，注册顺序/拓扑排序的关键边（未满足即拒注）；
- `DepCall`/`DepSoft`：调用期/软依赖，不影响装载序。

## 8. WASM ABI

### guest 导出（`egop_*`）

| 导出 | 用途 | 是否必须 |
|---|---|---|
| `egop_host_alloc` | 宿主写入参数字节前的分配函数 | 必须 |
| `egop_meta` | 无内嵌清单段时的清单来源（返回裸 Manifest JSON） | 条件 |
| `egop_init` | 注册完成后调一次 | 可选 |
| `egop_call` | 声明了 functions 时必需 | 条件 |
| `egop_tool` | 声明了 tools 时必需 | 条件 |
| `egop_apply_config` | 可下发配置 | 可选 |
| `egop_on_event` | 事件推送回调（入参是完整 `contract.Event` JSON） | 可选 |
| `egop_shutdown` | 卸载钩子 | 可选 |

参数/返回约定：字符串 = guest 内存 `(ptr,len)` 成对；除 `egop_meta`（裸 JSON）与
`egop_on_event`（无返回）外，均返回结果信封 `(ptr,len)`。事件/过滤统一：
`publish_event` 入参 = 完整 `contract.Event` JSON，`subscribe_event` 入参 = 完整
`contract.EventFilter` JSON（与进程内 `Surface.Publish`/`SubscribeEventFilter` 同构）。

### 宿主注入（module `egop`）

函数名即能力名：`call` / `get_setting` / `persist_read` / `persist_write` /
`persist_list` / `kv_get` / `kv_put` / `kv_delete` / `kv_keys` / `exec` /
`op`（通用扩展：op 名 + 入参）/ `publish_event` / `subscribe_event` / `on_hook`
（hook）/ `plugins` / `get_plugin`（plugin.meta 目录/元数据）/ `get_config` /
`set_config`（config.read/write 跨插件配置）/ `read_asset` / `log`。

返回 `i64` = `(len<<32)|ptr`，指向经 `egop_host_alloc` 分配的结果信封。

### 清单来源

- `.egop.wasm`：优先读自定义段 `egop.manifest`；缺省回退 `egop_meta` 导出。
- `.egop.zip`：`manifest.json` + `plugin.wasm` + `assets/*`。

## 9. 远程通道帧（loader/remote）

远程通道不再绑定传输：egop 只在注入的 `remote.Stream` 上收发 **JSON 帧**，payload
一律结果信封 JSON（无第二种编码）。帧结构即 `loader/remote.Frame`：

`kind`：`register` / `call_func` / `tool` / `hook` / `apply_config` / `host_call` /
`subscribe` / `push_event` / `shutdown` / `ping`。

- 请求/回复按 `id` 关联；单向帧（push_event / shutdown）`id=0`；
- 插件→框架：`HostCall`（能力回程，op 词汇同 wasm 宿主注入）+ `Subscribe`（帧内
  承载完整 `contract.EventFilter`）；
- 框架→插件：`CallFunc` / `Tool` / `Hook` / `ApplyConfig` / `PushEvent` / `Shutdown`——
  `PushEvent` 帧承载完整 `contract.Event`（Type/SubType/Labels/Source/Payload）；
- 传输实现（http/https/websocket/裸字节流）由外部注入，`remote.BindStream` 做帧化。