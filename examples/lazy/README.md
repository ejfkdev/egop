# examples/lazy

懒注册 + 批量装载示例：依赖「后到」自动补载、批量乱序按依赖拓扑排序。运行：

```sh
go run .
```

要点：

- `RegisterLazy(p)` 依赖未满足时返回 `StatusPending` 并转入待补载队列，不报错；
  后续 `Register`/`RegisterMany`/`Replace` 使依赖到位即自动补载（含传递链）；
- `RegisterMany(plugs)` 按 `Requires`（DepInit）拓扑排序后依序注册，缺依赖/成环/
  重复 id 的件**单独失败**、不阻断其余；依赖待补载插件的条目转 `rep.Pending`。

对照见 `examples/dependency`（依赖声明 + 跨插件调用）、`host/batch.go`
（`RegisterMany`/`RegisterStatus`）。