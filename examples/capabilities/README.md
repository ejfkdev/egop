# examples/capabilities

能力面（`contract.Surface`）示例：先说后做 + 扩展能力。运行：

```sh
go run .
```

要点：

- 插件声明一个个 capability，宿主按声明**裁剪**注入的 `Surface` 视图——未声明就
  调用会报错（`Op` 尤其如此）；
- 扩展能力走 `Surface.Op(name, input)`：`Options.Ops` 注册"守卫能力词 → 处理器"，
  `Options.OpAliases` 把 wire 短名映射到守卫词（内容无关，业务词由装配注入）；
- `Surface` 其余固定方法见 `doc/contract.md` §6（Call/GetSetting/Persist/KV/
  Exec/PublishEvent/SubscribeEvent/OnHook）。

对照见 `examples/config`（schema 校验）、`examples/hooks`（Hook）。