# examples/rawconn

**传输无关通道**示例：插件通道跑在一条裸 `net.Conn`（纯字节流）上。运行：

```sh
go run .
```

要点：

- `remote.BindStream(ctx, rw)` 把任意 `io.ReadWriteCloser` 包成 `remote.Stream`；
- 框架侧 `remote.ServeStream`/`remote.DialStream`、插件侧 `remote.AttachStream`/
  `remote.ServePluginStream` —— egop 只在这条流上收发 `Frame`，不建连接；
- 换 websocket / http 双向体 / 浏览器消息通道，只是把它们的双向流 `BindStream`
  一下，代码不变。

双向注册（入站/出站）同一条 JSON 帧表：框架侧 `remote.ServeStream`/`remote.DialStream`、
插件侧 `remote.AttachStream`/`remote.ServePluginStream`。