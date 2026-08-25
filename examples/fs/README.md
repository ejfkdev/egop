# examples/fs

跨平台只读注入缝示例：插件加载不依赖 OS 文件系统。运行：

```sh
go run .
```

要点：

- `wasm.LoadFS(ctx, bytes, name, opts)` 直接吃字节（浏览器拿到字节即可用）；
- `wasm.ScanFS(ctx, fsys, opts)` 遍历任意 `io/fs.FS`（内存/自定义实现），OS 目录
  只是 `os.DirFS` 的便捷默认；`mount.Sources.FS` / `autoload.Options.FS` 同理；
- 网络（远程通道）经 `Sources.StreamDial/StreamAccept` 注入；插件**出站网络**
  （调 LLM API 等）经 `host.Options.Net` 注入（`net.access` 能力，且目标须是
  网络协议，`file://` 被协议门拒绝）——本库不内置任何文件/网络底层操作。

夹具 `testdata/demo.wasm` 是 `loader/wasm/testdata/demo.wasm` 的副本（与库同源）。
详见 `doc/contract.md` 与主 `README.md` 的"跨平台与浏览器"一节。