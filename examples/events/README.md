# examples/events

事件总线（发布/订阅）示例：先说后做 + 固定事件结构 + 过滤匹配。运行：

```sh
go run .
```

要点：

- **固定事件结构**：`contract.Event{Type, SubType, Labels, Payload}` 是发布/订阅的统一载体；
  发布用 `Surface.Publish(ctx, Event{...})`，`Source`（来源身份/类别/点位/时间戳）与
  `Version` 由框架回填；`Surface.PublishEvent(ctx, topic, payload)` 是「无标签」的便捷面；
- **过滤订阅**：`Surface.SubscribeEventFilter(EventFilter{...}, fn)`——未设字段 = 不约束；
  可匹配主题（含 `*` 通配，如 `chat.*`）/`SubType`/来源插件 `SourceID`/来源类别
  `SourceKind`/标签 `Labels`（子集相等）；`SubscribeEvent(topic, fn)` 等价于
  `EventFilter{Type: topic}`；
- **边界**：只匹配事件的结构化字段，`Payload` 是不透明 JSON，不做深层解析匹配；
- **后端**：默认 `Options.Events = MemEvents`（内存、同步扇出、**不持久化**）。

对照见 `examples/origin`（事件来源溯源）、`doc/contract.md` §事件。