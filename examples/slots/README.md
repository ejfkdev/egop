# examples/slots

**槽位契约（Slot / SlotSpec）**示例：把"某个能力 id → 最小契约"注册进
`SlotLookup`，插件用 `Meta.Slot` 声明实现该 id，宿主逐轴求差（八轴，只多不少）。
运行：

```sh
go run .
```

演示点：

- **注册契约**：`Options.SlotLookup(id)` 返回一个 `contract.SlotSpec`，声明该槽位
  要求实现者具备哪些事件主题（`events`）/ 函数（`functions`）/ 能力等（八轴 + `needs`）。
- **声称实现**：插件在 `Meta.Slot` 填该 id。
- **逐轴求差**：不满足任一轴（如缺函数、缺事件主题）即拒注，错误逐轴列出；
  满足则入册，可正常按契约调用。

八轴清单见 [`doc/contract.md`](../../doc/contract.md) §4。相关机制内嵌示例见
`examples/dependency`（批装载 + 槽位契约），跨形态参考 `examples/hooks`、
`examples/tools`。