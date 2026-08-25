# examples/dependency

进程内两个插件的"依赖 + 跨插件调用"示例。运行:

```sh
go run .
```

演示点:

- **依赖顺序敏感**:`calc.sigma` 声明 `requires:{"deps":[{"plugin":"calc.basic","kind":"init"}]}`,
  先注册它会因 `calc.basic` 未在册被拒（首行输出）。
- **能力门控下的跨插件调用**:`calc.sigma.sum` 经 `contract.Surface.Call` 调
  `calc.basic.add`，开箱条件是其声明了 `plugin.call` 能力。
- **批量自动排序**:`Host.RegisterMany` 把"依赖方在前"的一批插件按依赖拓扑
  排序后装载；缺依赖的件单独失败、不阻断同批其余件。
- **依赖反查与卸载**:`Dependents` 看谁在依赖；被依赖时非级联卸载 fail-closed，
  级联卸载连带清退依赖者。
- **槽位契约**:`SlotLookup` 注册 `id → SlotSpec`（八轴最小契约），声称实现该
  id 的插件逐轴求差——不满足（如缺事件主题/缺函数）即拒注。
- **Hook 派发**:`OnHook` 注册回调、`TriggerHook` 触发，回调返回 `HookResult`
  （Block/Data）精确表达阻断与产出；插件未显式撤销的订阅/hook 在卸载时自动回滚。

插件作者侧参考:`examples/tools`(工具面)、`examples/rawconn`(远程通道)、
`examples/wasm`(WASM ABI)；批量装载核心见 `host.RegisterMany` 与
`host/batch_test.go`。