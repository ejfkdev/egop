# examples/hotreload

**热更（目录装载 + Watch）**示例：一个 wasm 插件目录被轮询监视，文件增/删映射为
注册/卸载，文件改映射为热替换（内容 hash 两段确认，替换失败回退保旧版）。运行：

```sh
go run .
```

要点：

- `mount.Sources{Watch:true, Interval}` 开轮询；`rt.Events()` 流出
  `register/replace/remove/failed` 事件；
- 本示例内嵌 `demo.egop.wasm`（`//go:embed`，清单来自自定义段 `egop.manifest`，
  插件 id `wasm.demo`），写入临时目录演示「写入→注册→删除→卸载→再写入→再注册」；
- 依赖排序/批量装载见 `examples/dependency`；传输无关远程通道见 `examples/rawconn`。