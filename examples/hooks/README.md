# examples/hooks

Hook 派发示例：注册回调 → 触发 → 每个回调返回一个 `HookResult`。运行：

```sh
go run .
```

要点：

- `Host.OnHook(hookID, fn)` 注册（返回撤销函数），`Host.TriggerHook(ctx, hookID, data)` 触发；
- 回调**写** `Block`（阻断）/ `Reason`（理由）/ `Data`（产出数据），框架**填**
  `Origin`（ID/版本/`Kind:"hook"`/`Point`/`At` 统一溯源）+ `Seq`（顺序）——阻断不再靠返回 nil/false 猜；
- 插件侧经 `Surface.OnHook` 注册的回调，在该插件卸载/热替换时由宿主自动回滚；
- remote/wasm 插件也可注册（`on_hook` HostCall op / 宿主注入 `on_hook`），与 Go 侧同语义。

对照见 `examples/dependency`（依赖/批量/槽位契约）。