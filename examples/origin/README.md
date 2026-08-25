# examples/origin

**溯源（Origin）**示例：事件订阅者经 `e.Source`、被调函数经 `contract.OriginFrom(ctx)`
拿到「谁 / 哪个版本 / 什么类型（event/hook/call/host）/ 哪个点位」的来源。运行：

```sh
go run .
```

要点：

- `pub`（声明 `event.emit`）发布事件 → 订阅者 `e.Source` 是 `Origin{ID:"pub", Kind:"event", Point:"topic.src"}`。
- `obs`（声明 `plugin.call`）跨调 `echoer.who` → `echoer` 里 `contract.OriginFrom(ctx)` 拿到 `Origin{ID:"obs", Kind:"call", Point:"who"}`。
- 事件与调用**共用同一个** `contract.Origin` 结构体，只是投递通道不同（广播走 `Event.Source` 字段，直调走 ctx）。

同目录 `origin_test.go` 是一段冒烟测试（`go test ./examples/origin`）。