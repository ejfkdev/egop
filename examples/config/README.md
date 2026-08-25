# examples/config

配置（Schema 校验 + ConfigProvider 权威读回 + 字段默认值）+ 函数 schema 校验示例。运行：

```sh
go run .
```

要点：

- `Meta.Provides.Config` 声明的字段，`SetConfig` 用各自 JSON Schema 校验，不合规即拒；
- `FuncSpec.Input/Output` 声明的函数，`Host.Call` 默认校验入参/返回（入参调用前拒、
  返回调用后拒），`Options.DisableFuncValidation` 全局关闭；
- `ConfigFieldSpec` 可带 `Default`（声明级默认值，供 UI 展示/回填）与 `Secret`
  （敏感字段脱敏提示）；
- 插件实现 `ConfigProvider`（`Config() json.RawMessage`）后，`Host.EffectiveConfig`
  优先读它的**权威生效配置**（含默认值补齐/归一化/脱敏），未实现回退 `Host.AppliedConfig`；
- `Host.SetConfig` 是整对象替换，`Host.SetConfigField`（或 `Surface.SetConfig`）是**单字段合并**；
- `schema` 支持 `type` 单类型/数组、`anyOf`（多格式）——见 `doc/usage.md` §6 或
  `doc/api.md` 的 schema 小节。

对照见 `examples/capabilities`（能力面）、`examples/hooks`（Hook）。