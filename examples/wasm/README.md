# examples/wasm

最小 WASM 插件（无函数,仅"目录存在"级注册演练）:

```sh
/opt/homebrew/Cellar/wabt/1.0.41/bin/wat2wasm echo.wat -o echo.egop.wasm
# 把 echo.egop.wasm 放进宿主插件目录(默认 ./plugins 或插件目录设置键),
# 宿主 ScanDir 发现即注册——清单来自 egop_meta 导出的内置 JSON。
```

进阶形态见 `loader/wasm/testdata/demo.wat`（函数/KV/跨插件调用/事件订阅/工具
的全 ABI 参考夹具）。