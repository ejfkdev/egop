# 远程插件通道（loader/remote）

进程外插件通道，**传输无关**：egop 只负责在一条注入的 `remote.Stream` 上收发
**JSON 帧**，不建立、也不依赖底层连接——http/https/websocket/裸字节流/浏览器
消息通道由外部注入（与 `io/fs.FS` 注入缝同理）。payload 一律不透明 JSON 字节，
语义即 `contract` 既有类型约定，结果信封与 WASM 插件 ABI 同构。

## 帧格式（唯一编码：JSON）

`Frame` 平铺为 JSON 字段；请求/回复按 `id` 关联，`payload` 承载结果信封。

```
Frame { id uint64; reply bool; kind string; error string; payload bytes;
        manifest / token / fname / input / name / tctx / hook_id /
        op / topic / config / reason }   // 各操作参数按 kind 平铺
```

`kind`：`register / call_func / tool / hook / apply_config / host_call /
subscribe / push_event / shutdown / ping`。

- 请求/回复按 `id` 关联；单向帧（`push_event`/`shutdown`）`id=0`；
- 回复统一信封：`payload={"ok":true,"result":…}|{"ok":false,"error":…}`，
  `error` 字段只用于传输级/校验失败；
- **HostCall op 词汇**（插件→框架能力回程，与 wasm 宿主注入同构）：
  `call / get_setting / persist_read / persist_write / persist_list / kv_get /
  kv_put / kv_delete / kv_keys / exec / on_hook / publish_event / plugins /
  get_plugin / get_config / set_config`——
  框架侧一律经 `RemoteHost.SurfaceFor(pluginID)` 取能力门控视图，未声明能力 =
  拒绝（与进程内插件同一语义）。其余 op 名经 `Surface.Op` 扩展透传，守卫词由装配
  注入（`host.Options.OpAliases`）。

`remote.BindStream(ctx, rw)` 把任意 `io.ReadWriteCloser` 转成 `remote.Stream`（4 字节
大端长度前缀 + 载荷）。要换传输，提供这组读写字节即可，代码不变。

## 两个连接方向（同帧表，只差谁先 Register）

**1. 框架主动向外连接（出站）**：外部传输给框架一条已连接的流 → `remote.DialStream`
先发 `Register`（manifest 空）→ 插件以 `Register(manifest=最终清单)` 回执 →
`DialOptions.WantID` 校验清单 id 防连错 → 注册进 Host，同流双向复用。

**2. 插件实例主动连接框架（入站）**：`remote.ServeStream` 收 `Register(manifest+token)`
→ 校验（可选 `token` 帧级口令）→ 回执 → **注册流双工复用**：插件→框架的
HostCall/Subscribe 与框架→插件的 CallFunc/Tool/Hook/ApplyConfig/PushEvent 全走
这一条流，插件**无需监听端口**（NAT/容器友好）。断流 = 自动 `Remove`（有 DepInit
依赖者由 Remove 的 fail-closed 逻辑告警保留），重连可重注册。

插件侧对称面：入站用 `remote.AttachStream`，被拨入（出站方向）用
`remote.ServePluginStream`。

## Go 插件作者示例（入站，10 行起步）

```go
// 先由外部 transport 建立一条流(如 net.Conn/websocket/浏览器通道),再 BindStream:
stream := remote.BindStream(ctx, conn)
sess, err := remote.AttachStream(ctx, stream,
    contract.Manifest{Meta: contract.Meta{ID: "my.id", Functions: []contract.FuncSpec{{Name: "greet"}}}},
    &remote.PluginOps{
        CallFunc: func(ctx context.Context, fname string, in json.RawMessage) (json.RawMessage, error) {
            return json.RawMessage(`"hello"`), nil
        },
        PushEvent: func(ctx context.Context, topic string, e contract.Event) { /* 命中过滤条件的事件推送(完整 Event) */ },
    })
// 插件→框架能力回程与订阅(过滤条件与进程内同构;nil/零值=全部):
out, err := sess.HostCall(ctx, remote.OpGetSetting, json.RawMessage(`{"key":"app.name"}`))
err = sess.Subscribe(ctx, &contract.EventFilter{Type: "dyn.some.topic"})
<-sess.Done() // 直至断开
```

`token` 是**帧级握手口令**（进 `Register` 帧，框架侧 `ServeStream(..., token, ...)` 校验），
与 HTTP 请求头无关——WebSocket/HTTP 的连接头等传输参数在外部建立 `Stream` 时由
transport 自由设置，egop 不接管。插件要带口令用
`AttachStream(..., WithToken("..."))`（`AttachStream` 不变参即空口令）。

## 边界

- TLS/mTLS 不属于本包——由外部传输实现（本包只管收发帧）；
- 无自动重连：断线 = 卸载 + 告警（重连 = 重新注册）；
- 同一连接 = 一个插件实例（多 id 多连）；
- **同一会话内禁止同步重入**：请求/单向帧由唯一的接收循环按序分发，处理函数
  （函数/工具/hook/HostCall/ApplyConfig）内若**同步**再走同一 `Session` 发请求并等回，
  回包路由会卡在同一条循环里造成死锁（直到 ctx 超时）。需要重入时另起 goroutine，
  或依赖 ctx 超时兜底。