# 设计决策记录

这里记录 egop 里几处**有意为之**的取舍——它们看起来像「缺口」，其实是边界/语义的显式选择。贡献者与使用者在改动前请先读这里，避免把决策当 bug 改回去。

## 1. 事件扇出是同步的，不是异步队列

`MemEvents.Dispatch` 会阻塞到所有订阅者回调返回。不引入缓冲队列/背压/丢弃策略，因为插件回调通常是轻量的状态更新，同步语义更可推理、也没有 goroutine 泄漏与顺序问题。要异步解耦，由装配层的 `Events` 实现自行选择。

## 2. 投递的事件是共享只读的，不深拷贝

同一 `Event` 值扇出给多个订阅者，`Labels`（map）与 `Source`（指针）是引用共享。订阅者必须按只读使用，不得改写（否则污染其它订阅者与发布者）。深拷贝 per-subscriber 性价比低，故只文档约定、不实现。

## 3. 事件过滤只匹配结构化字段，不解析 Payload

`EventFilter` 匹配 `Type`（通配）/`SubType`/`SourceID`/`SourceVersion`/`SourceKind`/`Labels`。
`Payload` 是不透明 JSON，不参与匹配——一旦要「按 payload 内部字段 match」就变成 JSONPath/查询引擎，
破坏「内容无关、零解析」这条立身之本；更深的匹配留给订阅方在回调里自己解。

## 4. `ctx` 不跨边界（远程/wasm 无 ctx）

`context.Context` 是进程内活体（deadline/取消通道/value 树），无法经 wire/ABI 序列化。
能过边界的是**数据**（Event JSON），不是 ctx。所以远程插件回调的 `ctx` 是**本侧会话上下文**、
wasm guest 无 ctx——事件里真正有用的存根（`Source`/`Labels`/`Payload`）都在 `Event` 本体上。

## 5. `egop_init`/`egop_shutdown` 用固定超时，不走 ctx

`Surface.SetSurface` 无 ctx（`Register` 链也无 ctx），`egop_init` 用 `initTimeout=30s` 固定兜底；
`egop_shutdown` 在 `Close(ctx)` 里再包 `shutdownTimeout=10s` 上限。wasm guest 是纯计算
（无 fs/网络注入），固定超时已足够接住死循环；为「调用方可控 deadline」去动整条
`Register`/`SetSurface` 注册链是 breaking 且收益低。

## 6. 运行时事件 topic 不自动命名空间

声明侧有 `EventID`/`PointID`（`dyn.<plugin>.<topic>` 去冲突），但运行时 `Publish(ctx, topic, …)`
是自由裸字符串，不自动加插件前缀。这是「任意主题」语义的代价；跨插件同名主题需插件作者
自觉用 `EventID` 命名。强制前缀化会抹掉自由主题能力，故维持约定。

## 7. `Host.Remove`/`Replace` 不调用 `Disposer`

`Disposer.Close` 仅 `Host.Close` 统一调用；单件 `Remove`/`Replace` 只清目录与 effect 栈，
**不** dispose 原生资源——调用方须自行 `Close` 插件句柄（`autoload` 正是如此：卸载/热替换后
立即 `plugin.Close`）。这样 `Remove`/`Replace` 无需 ctx，优雅关停的 ctx 也仍由调用方掌握。

## 8. `Surface.GetSetting` 无能力门控

能力词清单里没有 settings 读词：settings 是只读的宿主共享环境值，无隔离诉求，故不做
「先说后做」。与 `plugin.meta`/`config.read`/`net.access` 等受门控的能力不同，这是刻意简化。

## 9. 能力门控与八轴求差是「最小契约」

`SlotSpec` 逐轴求差是「只多不少」的**最小**契约（声明面 ⊇ 槽位要求），不是相等校验。
插件可以比槽位要求提供更多。