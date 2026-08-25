# 贡献指南

egop 是**内容无关的插件管理 Go 库**（MIT）。任何改动都以「不破坏内容无关分层与跨世界
JSON 契约」为前提，并保证 `make hygiene` 全绿。

## 快速上手

```sh
make hygiene        # gofmt + go vet + go test ./...（任何改动后必跑）
go test -race ./... # 可选：竞态检测
```

需要 Go 1.26+。

## 自检清单

- `make hygiene` 全绿；
- 新行为有包级测试，且用完即净（`t.TempDir`，无残留）；
- 测试不 mock 骗绿：loader 测试用真 wazero 实例、真 `net.Pipe` 字节流，热更测试用真实
  临时目录文件；
- 改动面同步到 `README.md` / `doc/`（新增轴、命令、坑）；
- 改 `HostFace` 等对外签名时，评估外部消费仓联动（见 `AGENTS.md` 验收标准）。

## 分层与不变量（摘录，全文见 `AGENTS.md`）

包分层：

```
contract ← 词汇真源（Meta/Manifest/SlotSpec/Surface/Event/能力词），仅 stdlib
loader   ← HostFace（统一宿主面），仅依赖 contract
schema   ← 预设结构目录 + 校验，仅 stdlib+reflect
host     ← 泛化宿主 Host[C]，依赖 contract+schema
undo     ← effect 栈，无依赖
exchange ← 信封翻译表，依赖 contract
loader/wasm / loader/remote ← contract+loader+schema
autoload / mount            ← contract+loader+loader/wasm(+remote)
skeleton / examples         ← 参考骨架 / 可跑示例
```

关键不变量：

- 唯一编码：跨边界全程 JSON；WASM ABI 与远程帧的结果信封同构
  `{"ok","result","result_b64","error"}`；
- 能力门控「先说后做」；函数 schema 校验默认开；
- 工具在 loader 侧无类型（`ToolRaw`），typed 包装在消费方装配层；
- 热更两段确认 + 替换失败回退；`Host.Close` 逆注册序 + Disposer 清退；
- 批量装载按 `Requires` 拓扑排序 + 失败隔离。

设计取舍见 [`doc/decisions.md`](doc/decisions.md)（事件同步扇出、不深拷贝、过滤不解析
Payload、ctx 不跨边界、固定超时等）。

## 提 PR 前

1. 逐包 `go build ./...` + 全量测试核对；
2. 若改了 `doc/` 里描述的行为，同步文档；
3. 说明动机与影响面（是否 breaking、是否触及 ABI/帧格式/对外签名）。