# examples/collab

**跨插件协作**示例：一个插件通过 `plugin.meta` 读「已加载插件列表 + 其它插件元数据」，
通过 `config.read`/`config.write` 读写另一个插件声明的配置字段（受 `ConfigFieldSpec`
的 `Readable`/`Writable` 字段级标志 + 能力词双重门控）。运行：

```sh
go run .
```

要点：

- `app.settings` 声明两个配置字段：`api_key`（`Writable:true, Readable:false`，别的插件
  能写、读不回）与 `mode`（`Readable:true`，只读）；egop 始终可读写。
- `app.worker` 声明 `plugin.meta` + `config.read` + `config.write` 后，经自己的
  `Surface`：`Plugins()` 列目录、`GetPlugin(id)` 读元数据、`GetConfig/SetConfig` 读写配置。
- `app.blind` 什么都没声明 → 目录/元数据/配置全不可见（空/`false`）。
- 批量装载用 `RegisterMany`；依赖排序见 `examples/dependency`；持久化注入见
  `doc/contract.md` 的「持久化注入」——浏览器/内嵌经 `Options.Storage` 注入后端。