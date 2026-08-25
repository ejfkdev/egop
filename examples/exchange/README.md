# examples/exchange

信封翻译表（`exchange`）示例：事件载荷的登记 / 打包 / 强类型解回。运行：

```sh
go run .
```

要点：

- `exchange.Register(name, proto)` 登记载荷类型（幂等；同名不同型 panic）；
- `exchange.NewEvent(point, payload, subTypeHint)` 打包成 `contract.Event`——`payload`
  传值或**指针**都归一为同一 `SubType`（类型名），并自动填 `Source{Kind:host}`；
- 订阅方 `exchange.DecodeAs[T](ev)` 直接解回强类型，`Decode` 回 `any` 需断言；
- 未登记的子类型 `Decode` 报错，不透传未知载荷。

对照见 `examples/origin`（溯源 Origin）、`doc/contract.md` §事件。