# examples/net

出站网络能力（`contract.Net`）示例：先说后做 + 请求族 / 消息流族。运行：

```sh
go run .
```

要点：

- 插件声明 `net.access` 能力，才能 `Surface.Net()` 取到出站网络后端（未声明返回
  `false`）——后端经 `host.Options.Net` 注入，egop 自身不实现任何传输；
- **单向请求族**走 `Net.Request`：HTTP/HTTPS/SSE/gRPC-Web/JSON-RPC/GraphQL/REST
  统一于此。SSE 是「长响应体」（`Response.Body` 持续读）；gRPC(-Web) unary 的尾部
  元数据在 `Response.Trailers`；
- **双向消息流族**走 `Net.DialStream`（返回 `Stream` 字节消息流）：URL scheme 决定
  传输——`ws`/`wss` 即 WebSocket、`https`(HTTP/3) 即 WebTransport、可自定义其它
  scheme，WebRTC DataChannel / MQTT-over-WS 也用同一形状；
- **网络协议门**：目标必须是网络协议（内置 `http`/`https`/`ws`/`wss`，
  `Options.NetSchemes` 可补充），`file://` 等本地/特殊 scheme 在转交装配实现前即被拒——
  见 `fetcher.leak`（试图读 `file:///etc/passwd` 被拒）；
- 本例注入一个**自包含的内存后端**（`memNet`，无真实网络）以便一处跑通；生产装配层
  用 `net/http`+websocket（桌面）或 `fetch`/`WebSocket`/`WebTransport`（浏览器 wasm）
  实现同一面。

对照见 `examples/capabilities`（能力面 + 扩展 Op）、`doc/contract.md` §出站网络。