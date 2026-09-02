# examples/codeless

**无代码插件（资源包）**示例：`.egop.zip` 布局缺 `plugin.wasm` 即「纯清单/资产」
形态——manifest 声明元数据与 `Extensions`（自由扩展键值），`assets/` 携带静态
资源，宿主经 `Meta()`/`Assets()` 消费，全程**不执行任何 guest 代码**。运行：

```sh
go run .
```

要点：

- **品牌后缀注入**：库内只认 `*.egop.wasm` / `*.egop.zip`；自有包后缀（本例
  `.pack.zip`）经 `wasm.Options.ExtraSuffixes` 装配注入（`autoload.Options` /
  `mount.Sources` 同名透传）——内容无关库不内置业务词，未注入即忽略。
- **Extensions 透传**：`Meta.Extensions` 任意 key → 任意 JSON，egop 不解释、
  不校验；消费方按自己的 key 读取（本例 `demo.entry` 指向入口资产）。
- **Assets 副本语义**：`Assets()` 返回资源表副本（含子目录键如 `theme/dark.css`），
  改副本不污染插件内部；资产名在拆包时已拒绝 `..`/绝对路径穿越。
- **fail-closed**：资源包声明函数/工具/配置/hook 点等需要代码兑现的面会在装载期
  被拒；即使绕过声明，`Call`/`SetConfig` 也是干净错误而非 panic。
- 资源包与代码插件**同一生命周期面**：注册/目录/事件/卸载一视同仁。

同目录 `codeless_test.go` 是冒烟测试（`go test ./examples/codeless`）。
对照见 `examples/fs`（io/fs 注入缝）、`examples/wasm`(有代码 wasm 插件）。
