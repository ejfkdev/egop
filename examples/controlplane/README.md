# examples/controlplane

宿主控制面（元数据反查）示例：只读查询口全景。运行：

```sh
go run .
```

要点：

- `Snapshot()` 返回纯净全景（plugins/functions/capabilities/applied_config，不含实例句柄），
  可 `json.Marshal` 给控制面 UI；
- `Dependents(id)` 反查依赖某插件的在册插件（卸载前 fail-closed 判断链路）；
- `CapabilityIndex()` 反查「能力词 → 声明者」；`Functions()` 全量函数目录；
  `Tools()` 工具面；`Plugins()` / `HasPlugin()` 目录与存在性。

对照见 `examples/inproc`（进程内全链路）、`host/controlplane_test.go`、`doc/api.md`。