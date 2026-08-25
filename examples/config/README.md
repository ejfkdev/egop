# examples/config

配置 + 函数 schema 校验示例。运行：

```sh
go run .
```

要点：

- `Meta.Provides.Config` 声明的字段，`SetConfig` 用各自 JSON Schema 校验，不合规即拒；
- `FuncSpec.Input/Output` 声明的函数，`Host.Call` 默认校验入参/返回（入参调用前拒、
  返回调用后拒），`Options.DisableFuncValidation` 全局关闭；
- `schema` 支持 `type` 单类型/数组、`anyOf`（多格式）——见 `doc/usage.md` §6 或
  `doc/api.md` 的 schema 小节。

对照见 `examples/capabilities`（能力面）、`examples/hooks`（Hook）。