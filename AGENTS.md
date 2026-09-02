# AGENTS.md — egop 维护者指南

给在本仓工作的 AI 会话与人类维护者。目标:任何修改**不破坏"内容无关"
分层与跨世界 JSON 契约**,并且 `make hygiene` 永远全绿。

## 库定位(改动前先自问)

egop 是**内容无关的插件管理 Go 库**(module `github.com/ejfkdev/egop`,
MIT)。它管理插件(注册/卸载/热替换/装载)与元数据契约、能力门控、ctx
能力面;它**不**知道 llm、agent、react 等任何业务概念——业务类型一律禁止
进库。对照对象是 cordis(见 README 对照表)。

## 包分层(事实约定,勿突破)

```
contract  ← 词汇真源:Meta/Manifest/SlotSpec/Surface/Event/能力词;仅 stdlib
loader    ← HostFace(统一宿主面,9 法);仅依赖 contract
schema    ← 预设结构目录 + 配置校验;仅 stdlib+reflect
host      ← 泛化宿主 Host[C];依赖 contract+schema
undo      ← effect 栈;依赖 struct{}
exchange  ← 信封翻译表;依赖 contract
loader/wasm / loader/remote ← contract+loader+schema + wazero(remote 仅 stdlib,传输注入)
autoload / mount          ← contract+loader+loader/wasm(+remote);绝无业务类型
skeleton  ← 参考骨架(非必经路径,改动需同步说明)
examples  ← 可跑示例,禁止示例里引入业务假设
```

禁止:contract→host、host→loader/remote、任何包→业务域、循环 import。改层间
依赖后必须 `make hygiene`(分层破坏往往仍能编译,靠全量测试与审视兜底)。

## 不变量(JSON 契约与机制语义)

- **唯一编码**:跨边界全程 JSON。WASM ABI 与远程通道帧(loader/remote)的结果信封同构
  `{"ok":bool,"result":any,"result_b64":string,"error":string}`。
- **能力门控**:先说后做。插件未声明 capability,Surface 视图直接不给/报错;
  new_op 进宿主必须挂守卫词(`Options.OpAliases` 可映射 wire 短名→守卫词)。
- **机制层故障隔离**(插件不可信→fail-closed):宿主在调用插件代码的边界
  (`Host.Call`/`SetConfig`/`Tool.Run`、hook 触发、事件订阅扇出、remote 入站
  dispatch)把 panic 归一到 error(或 HookResult.Reason/丢弃),绝不炸穿宿主进程;
  消费方不重复 recover。
- **函数 schema 校验默认开**:`Host.Call` 对声明 `FuncSpec.Input/Output` 的函数
  做 JSON Schema 校验(入参调用前拒、返回调用后拒),`Options.DisableFuncValidation`
  可整体关;`schema.Validate` 支持 `type` 数组与 `anyOf`(多格式),有测试固化。
- **元数据八轴双向**(README 表):Meta 声明面与 SlotSpec 最小契约逐轴求差
  (provides/capabilities/functions/hooks/config/events/listens/needs_tools +
  独立 needs 前置槽位),新增轴必须两边同时落(contract 字段 + host slotCheck
  求差 + README 表)。
- **工具在 loader 侧无类型**:`ToolRaw(name)(func(ctx,tctxJSON,args)
  (string,error),bool)`。tctx = 线上 JSON;typed 包装(tctx 序列化)永远在
  **消费方装配层**,禁止在 loader 里引任何会话上下文类型。
- **SubscribeEvent 回调带 topic**:`func(ctx, topic string, e Event) func()`。
- **事件/hook 投递不重入取锁**:总线同步扇出可能在"guest 自己正持实例锁调用中"
  的同一 goroutine 上重入投递(插件发布命中自身订阅的事件)——wasm 侧
  `pushEvent`/`invokeHook` 一律 `TryLock`,锁忙即跳过本次投递(事件丢弃/hook 记
  Reason,best-effort),绝不阻塞取锁(对非重入锁自死锁,有 selfpub 夹具测试固化)。
- **热更两段确认**:增/改连续两轮 hash 一致才应用(抗半截写),**删连续两轮未见
  才卸载**(抗目录瞬态读失败误卸);替换失败必须回退保旧版,且 `Replace` 过与
  `Register` 同款契约校验(DepInit 依赖/槽位八轴)——替换口不得比注册口宽松。
  这些语义有测试固化,勿"顺手简化"。
- **关停语义**:`Host.Close` 逆注册序 + Disposer 清退;Remove(cascade=false)
  被依赖时 fail-closed——点名依赖与**槽位依赖**同判(按"删除后槽位是否仍有其它
  供给"精确判定,槽位名与插件 id 是两个命名空间,勿直接相等比较);级联 victims
  去重,`plugin.removed` 每插件只广播一次。
- **批量装载**:`host.RegisterMany` 按 `Requires`(DepInit,Plugin/Slot) 拓扑排序
  后依序注册,失败隔离(缺依赖/成环/重复 id 单独失败);`mount` 首装拍至稳定、
  `autoload` 依赖后到自动补载——均有测试固化,勿回退成固定次数/固定顺序。

## 命令与工具链

```sh
make hygiene   # fmt+vet+全量测试:任何修改后必跑
# wabt 的 wat2wasm:/opt/homebrew/Cellar/wabt/1.0.41/bin/
```

改 WASM ABI 后:改 `loader/wasm/testdata/demo.wat` → wat2wasm 重编
`demo.wasm`(fixtures 双双入库;autoload、mount、examples 各自持有
testdata/demo.wasm 副本,同步重编无缝)。远程通道(loader/remote)不再有 proto/gen:
交换格式即 JSON `Frame`(见 `loader/remote/stream.go`),改帧字段亦须同步 `session.go`
收发与 `doc/contract.md` 的帧表。

## 已知坑(前人踩过,不要重踩)

- **wazero v1.12 API**:`WithGoModuleFunction` 收 `api.GoModuleFunc`;宿主
  注入函数结果写回 `stack[0]`(不是 append);内存上限在
  `RuntimeConfig.WithMemoryLimitPages`,不在 ModuleConfig。
- **gofix 级批量正则**:本仓无 git 回滚,批量 sed/正则替换后必须逐包
  `go build ./...`+全量测试核对(曾有一次毁掉一批测试文件的手工教训)。
- **测试不许 mock 骗绿**:loader 测试用真 wazero 实例、真 net.Pipe 双向字节流
  (远程通道);热更测试用真实临时目录文件写/改/删。
- **读插件文件一律走 io/fs.FS 注入缝**:`wasm.ScanFS` / `autoload.Options.FS` /
  `mount.Sources.FS` 承载读侧(目录发现/读取字节)解耦——勿在本库读路径重新引入
  `os.ReadFile`/`filepath.WalkDir`(js/wasm 虽能编译,运行时会失败)。"直接给内容"
  走 `wasm.LoadFS(字节)`;网络注入走 `host.Options.Net`(`Surface.Net()`,
  出站目标须是网络协议,`file://` 等被协议门拒绝);全局文件系统注入走
  `host.Options.FS`(`Surface.FS()`,`fs.read`/`fs.write` 分向门控,范围/沙箱由
  注入实现决定)。
- **品牌/业务词不进库**:插件包后缀只内置 `*.egop.wasm` / `*.egop.zip`,其它
  后缀(品牌 zip 等)经 `wasm.Options.ExtraSuffixes` → `autoload.Options` →
  `mount.Sources` 装配注入;后缀判定收敛在 `wasm.IsPluginFile` 单点,勿在
  扫描/装载侧各写一套。zip 解压有上限(`MaxEntryBytes`/`MaxTotalBytes`,防
  zip bomb),资产名拒绝 `..` 穿越——插件是不可信输入,勿移除这些防线。

## 验收标准(任何改动)

1. `make hygiene` 全绿;
2. 新行为有包级测试,且测试用完即净(t.TempDir,无残留);
3. 改动面在 README/AGENTS 里同步(轴/命令/坑);
4. 若改动了 HostFace 等对外签名:评估外部消费仓(eha,经 replace 指向本库)
   联动并通过其 hygiene——签名变更必须兼容或明确列为 breaking。

## 不做(Boundary,不新增)

TLS/mTLS、自动重连、fsnotify 式内核监视(轮询+两段确认已足够)、
非 wasm 文件热更、发布/版号流程(属宿主环境事务)。