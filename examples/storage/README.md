# examples/storage

插件专属持久化（`contract.Storage` 注入）示例：先说后做 + File/KV 两套存储面。运行：

```sh
go run .
```

要点：

- 插件声明 `storage.persist` / `storage.kv` 能力，才能 `Surface.Persist()` / `Surface.KV()`
  拿到**插件专属、按 pluginID 隔离**的存储面；未声明返回 `(nil,false)`；
- 后端经 `host.Options.Storage` **必填注入**（实现 `contract.Storage`），宿主只把原始
  pluginID 转发给实现——命名空间/目录布局/序列化由实现自行决定，egop 不越权；
- 本例注入一个**自包含的内存后端**（无真实文件/网络）；生产装配层可用真实文件系统、
  数据库或 KV 实现同一面。

对照见 `examples/fs`（wasm 目录读取的 `io/fs.FS` 注入缝）、`doc/contract.md` §持久化注入。