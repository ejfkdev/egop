# examples/fsaccess

全局文件系统能力面（`Surface.FS()`）示例：`fs.read` / `fs.write` **分向门控** +
后端策略沙箱。运行：

```sh
go run .
```

要点：

- 装配层注入 `contract.FS` 后端（`host.Options.FS`）——egop 不实现任何平台 IO；
  **可见范围/沙箱策略完全由实现决定**（本例的 `scopedFS` 只暴露 `shared/` 前缀）。
- 插件按声明拿裁剪视图（先说后做，宿主 `fsGuard` 单点强制）：
  - 只声明 `fs.read` → `ReadFile` 放行、`WriteFile` 报错；
  - 只声明 `fs.write` → 反之；
  - 都未声明 → `FS()` 返回 `(nil,false)`，拿不到面。
- 与 `storage.persist` 互补：那是**插件专属隔离目录**（宿主转发 pluginID），
  这是宿主文件系统的**显式受控视图**（策略在注入实现里）。
- 跨世界同构：wasm 插件走 ABI `fs_read`/`fs_write`、远程插件走 `HostCall`
  `fs_read`/`fs_write`，与进程内同一门控语义。

同目录 `fsaccess_test.go` 是冒烟测试（`go test ./examples/fsaccess`）。
对照见 `examples/storage`（插件专属存储）、`examples/capabilities`（Op 扩展能力）。
