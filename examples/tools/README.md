# examples/tools

工具面示例：一个插件声明 `tool.provide` 能力，把"返回字符串 + 带上下文"的
可调用体暴露给宿主。运行：

```sh
go run .
```

要点：

- 工具与函数共用同一种 spec（`contract.FuncSpec`），差别只在**执行形态**
  （返回字符串、多一个不透明上下文 `tctx`）；
- 是否进工具面由注册面决定：声明 `tool.provide` 能力 + 实现 `ToolProvider`；
- 宿主经 `Host.Tools()` 收集工具，`Tool.Run(ctx, tctx, args)` 执行并拿到字符串。

对照见 `examples/dependency`（普通函数 + 依赖 + 批量装载）与
`examples/rawconn`（远程通道；远程工具经 remote 帧 `Tool` 投递）。