# skeleton（参考骨架）

本目录是一份**结构参考骨架**，说明「一个插件宿主大概长什么样、分哪几块」。它是
**非必经路径**，各子包只用最小的独立原语演示一种骨架写法，不重复实现 core。

与最新 `contract` 的对应关系：

- `config`：**读面已对齐** `contract.Source`（`Source = contract.Source`，与
  `host.Options.Settings` 同源）；`Putter` 是本地补充的写面（contract 未定义设置写面，
  `host` 的 `MapSettings.Set` 是实装）。
- `loader`：通用 **DI/组件表**原语（`Service[T]` 类型化令牌 + `Registry/Map`）——与
  `contract` 的插件契约（Meta/Slot/函数面）是两层；插件生命周期本体见 `host`。
- `registry`：泛型 `K→V` 注册表原语，与 `host` 按 `Meta.ID` 的插件注册是两层。
- `trigger`：独立的条件触发/分发原语，与 `contract` 的事件总线
  （`Event`/`EventFilter`/`MemEvents`）是两层；事件总线本体见 `contract`+`host`。

要真实可用的插件宿主能力，请直接看 `host`、`loader`、`mount` 与 `doc/`。本目录仅作
设计速览，不作为任何 ABI 或兼容性承诺。