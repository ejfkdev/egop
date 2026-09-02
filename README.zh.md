# egop

> 本文件是中文版；英文默认版见 [README.md](README.md)。

[![ci](https://github.com/ejfkdev/egop/actions/workflows/ci.yml/badge.svg)](https://github.com/ejfkdev/egop/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-blue)](#安装)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**内容无关的插件管理 Go 库**（MIT,module `github.com/ejfkdev/egop`）：
插件的注册/卸载/热替换、元数据静态声明（八轴 + 能力门控）、四种装载形态
（进程内 / WASM 包 / 目录热更 / 远程通道）与 ctx 能力面。作为通用库零
装配即可跑通：`host.New` 自带内存事件总线与配置事件,`mount.Mount` 一个
调用拉齐全部外接面,业务类型（llm/react/agent 等）一律不进本库——宿主泛化
为 `Host[C]`,业务能力经装配注入（`Ops`/`OpAliases`/`ToolNames`）。

> **版本状态**：当前为 **v0.x（pre-1.0）**。API 尚未冻结，`go get` 请锁定具体
> commit/tag；跨次要版本升级前请对照 [doc/api.md](doc/api.md) 核对签名。

## 安装

需要 Go 1.26+（跨平台核心可编译到 `js/wasm` 与 `wasip1`，运行时能力由装配层注入）。

```sh
go get github.com/ejfkdev/egop
```

```go
import (
    "github.com/ejfkdev/egop/contract"
    "github.com/ejfkdev/egop/host"
)
```

## 快速开始

进程内形态——零装配，最小完整示例（可整段复制运行）：

```go
package main

import (
    "context"
    "encoding/json"
    "log"

    "github.com/ejfkdev/egop/contract"
    "github.com/ejfkdev/egop/host"
)

type hello struct{}

func (hello) Meta() contract.Meta {
    return contract.Meta{
        ID: "demo.hello", Name: "Hello", Version: "1",
        Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "greet"}}},
    }
}
func (hello) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
    return json.RawMessage(`"hi"`), nil
}

func main() {
    ctx := context.Background()
    h := host.New[any](host.Options[any]{Logf: log.Printf}) // 内存总线/settings/点位开箱即用
    if err := h.Register(hello{}); err != nil {             // 先注册
        log.Fatal(err)
    }
    out, _ := h.Call(ctx, "demo.hello", "greet", json.RawMessage(`{}`)) // 后调用
    log.Printf("greet() = %s", out) // greet() = "hi"
    defer h.Close(ctx)               // 逆序清退
}
```

目录热更 + 远程插件——一份 `mount.Sources` 声明拉齐全部外接面：

```go
rt, warns, err := mount.Mount(ctx, h, mount.Sources{
    Dirs:   []string{"./plugins"},     // *.egop.wasm / *.egop.zip(可选 Watch 热更)
    Remote: []mount.RemoteSpec{{ID: "x", Addr: "custom://x"}},  // 框架主动拨出
    StreamDial:   func(ctx context.Context, addr string) (remote.Stream, error) { ... },
    StreamAccept: func(ctx context.Context) (remote.Stream, error) { ... }, // 插件主动拨入
})
for e := range rt.Events() { /* register/replace/remove/failed 热更事件 */ }
defer rt.Close()
```

完整可跑示例（每目录均有 README，全表见 [`doc/examples.md`](doc/examples.md)）：

| 主题 | 示例 |
|---|---|
| 进程内 | `inproc`（全链路）、`dependency`（依赖/跨调用/批量/槽位）、`lazy`（懒注册+拓扑）、`slots`（槽位契约） |
| 能力面 | `capabilities`（门控+Op）、`config`（schema 校验）、`tools`（工具面）、`hooks`（Hook 派发）、`events`（事件总线）、`origin`（溯源）、`controlplane`（控制面反查） |
| 装载形态 | `wasm`（最小 ABI）、`hotreload`（热更）、`rawconn`（远程通道）、`collab`（插件元数据/配置） |
| 注入缝 | `fs`（fs.FS）、`storage`（插件持久化）、`net`（出站网络）、`exchange`（信封翻译表） |

## 文档

- [`doc/contract.md`](doc/contract.md) —— 契约词汇与跨世界不变量（信封/能力门控/槽位八轴/Origin/事件过滤/WASM ABI/远程帧）
- [`doc/api.md`](doc/api.md) —— 公共 API 签名速查
- [`doc/usage.md`](doc/usage.md) —— 按主题的最小用法片段
- [`doc/examples.md`](doc/examples.md) —— 全部可跑示例一览
- [`doc/decisions.md`](doc/decisions.md) —— 设计取舍记录（同步扇出、不深拷贝、ctx 不跨边界、固定超时等）
- [`CONTRIBUTING.md`](CONTRIBUTING.md) —— 贡献指南

设计参照 cordis 的三个机制并落地为 Go:

| cordis | egop | 说明 |
|---|---|---|
| service 注册表 + dispose 级联 | `host.Host[C]` Register/Remove(cascade) | 级联卸载 + fail-closed |
| schema（配置静态校验） | `schema`（预设结构目录 + 配置校验） | 声明即校验,未声明未下发 |
| ctx 注入 + effect 栈 | `contract.Surface`（能力门控视图）+ `undo.Catcher` | 订阅/清理统一 effect 栈 |
| loader 配置装载 + hmr | `mount` + `autoload` | 目录增/改/删 = 注册/热替换/卸载,坏包不回退场景见热更新节 |

## 包结构

- `contract`——元数据契约（`Meta`/`Manifest`/`SlotSpec`/事件/能力词常量）与
  `Surface` 接口族。**类型真源**（Meta 的 NeedsTools / SlotSpec 的 NeedsTools 组成八轴）。
- `host`——泛化宿主 `Host[C any]`：注册/卸载/热替换、函数目录、配置 Schema
  校验、槽位八轴求差、Needs 能力依赖、`SurfaceFor`（远程插件
  能力回程路由入口）、批量装载 `RegisterMany`（按 DepInit 依赖拓扑排序）。
- `schema`——预设结构目录（注册 Go 结构作为参考形状）+ 配置/函数入参返回的通用
  JSON Schema 子集校验（`Validate`,支持 `type` 数组 / `anyOf` 多格式）。
- `loader/wasm`——`*.egop.wasm`（自定义段 `egop.manifest` 内嵌清单,缺省回退
  `egop_meta` 导出）与 `*.egop.zip`（manifest.json+plugin.wasm+assets/;plugin.wasm
  可缺省——无代码插件:纯清单/资产,如 UI 插件）加载;品牌 zip 后缀经
  `Options.ExtraSuffixes` 装配注入;目录递归发现 `ScanDir`。
- `loader/remote`——远程通道(传输无关):egop 只在注入的 `remote.Stream` 上收发 JSON
  帧,连接由外部建立;框架主动拨插件 / 插件主动拨框架同帧表,注册流双工复用。
- `loader`——统一宿主面 `loader.HostFace`:装配组件(autoload/mount/remote)
  只依赖它,`host.Host[C]` 天然满足,非 core 宿主包一层桥即接入全部加载器。
- `autoload`——**热更目录装载器**:轮询+hash 判变+两段确认,增/改/删映射为
  注册/热替换/卸载;替换失败回退保旧版;替换成功重放已下发配置。
- `mount`——**一站式装配**:一份 `Sources` 声明(插件目录[可选热更]+
  远程出站 + 入站 accept,传输经注入)同时驱动全部外接面,业务仓零加载循环。
- `skeleton/{loader,config,registry,trigger}`——机制骨架(装配参考实现)。
- `undo`——统一 effect 栈。

## 元数据声明轴（双向语义）

`contract.Meta` 的字段按方向分三组;`SlotSpec` 是"业务模块槽位"的最小契约,
实现声称该槽位时逐轴求差（只多不少）:

| 轴 | Meta 字段 | SlotSpec 字段 | 语义 |
|---|---|---|---|
| 发射点位 | `provides` | `provides` | 插件保证发射的框架点（宿主 EnsurePoint） |
| 自有 hook | `hooks` | `hooks` | 插件对外 hook 点（modify/observe） |
| 事件主题 | `events` | — | 对外发布的事件主题（完整声明含载荷 Schema） |
| 函数面 | `functions` | `functions` | 可调用函数目录（函数目录 + Schema） |
| 能力面 | `capabilities` | `capabilities` | 先说后做:Surface 视图按此裁剪 |
| 配置面 | `config` | `config` | 可下发配置字段（键+Schema,下发即校验） |
| 可监点 | `listens` | `listens` | 插件要订阅的框架点（框架须先保证存在） |
| 前置槽位 | `requires.deps`（富形态） | `needs`（槽位名清单） | 同主题形状不同:槽位前置是名字清单(运行时满足校验,非逐字段求差),插件自身依赖是富 `Dependency`(slot/kind/version)——保留异名 |
| 工具依赖 | `needs_tools` | `needs_tools` | 需要框架提供的工具名（就位校验见下） |
| 声明即生效 | 辅助:导出/副触发 | `tool.provide` | 是否向工具面提供工具（CapTools） |

**框架就位校验**：插件 `needs_tools` 声明的每个名字必须有供给
（`Options.ToolNames()` 或任一在册插件提供）;`SlotSpec.NeedsTools ⊆ Meta.NeedsTools`
由槽位求差保证。

**函数入参/返回校验（默认开）**：`Host.Call` 对声明了 `FuncSpec.Input` / `Output`
的插件函数做 JSON Schema 校验——入参不合规在调用前拒绝、返回不合规在调用后
拒绝（`Options.DisableFuncValidation` 整体关闭）。`schema.Validate` 支持 `type`
单类型或类型数组、`anyOf`,覆盖"一个字段允许多种格式"（如 int 或 string）。

## 加载形态

```go
h := host.New[MyCtx](host.Options[MyCtx]{...})

// 1. 进程内:直接实现 contract.Plugin
h.Register(&myPlugin{})

// 2. WASM 包:.egop.wasm/.egop.zip(目录递归发现)
plugs, errs := wasm.ScanDir(ctx, "./plugins", wasm.Options{})

// 3. 远程:egop 不建连接,传输注入(把已建立的流 BindStream 后交给我门)
adapter, sess, err := remote.DialStream(ctx, rh, stream, remote.DialOptions{WantID: "x.id"}) // 框架主动拨出
_ = remote.ServeStream(ctx, rh, stream, "", nil)                                           // 插件主动拨入
```

进程内与远程插件在宿主里同一套生命周期、同一套能力门控。WASM 插件 ABI 与
远程通道帧信封同构：`{"ok":bool,"result":any,"result_b64":string,"error":string}`
——全程 JSON,无第二种编码。

批量装载无需手工排序：把一批插件（如 `wasm.ScanFS` 的返回）交给
`host.RegisterMany`,框架按 `Requires` 的 `DepInit` 依赖（点名或槽位）自动拓扑
排序后依序注册；缺依赖、成环、重复 id、槽位契约不满足的件单独失败，其余照常。
依赖**后到**场景用 `host.RegisterLazy`：依赖未满足先入待补载队列，待依赖注册后
自动补载（含传递链）。

## 热更新（autoload,对标 cordis 的 loader+hmr）

```go
rt, warns, err := mount.Mount(ctx, hf, mount.Sources{
    Dirs:     []string{"./plugins"},
    Watch:    true,             // 目录增/改/删 → 注册/热替换/卸载
    Remote:   []mount.RemoteSpec{{ID: "x", Addr: "127.0.0.1:7401"}},
    StreamAccept: func(ctx context.Context) (remote.Stream, error) { ... }, // 插件主动拨入框架(注册流双工复用)
})
for e := range rt.Events() { /* register/replace/remove/failed 事件 */ }
defer rt.Close()
```

安全语义（与 cordis 的失败隔离/effect 清退对齐）：

- **内容 hash 判变 + 两段确认**:连续两轮 hash 一致才应用,天然抗半截写入;
- **替换失败回退保旧版**:新包关闭丢弃,旧版继续服务,仅事件告警;
- **替换成功重放配置**:已下发配置(AppliedConfig)在新版上重放;
- **坏包隔离**:单个坏包/重复 id 告警跳过,不阻断其它插件;
- **删除被依赖者**:Remove fail-closed(宿主裁决),事件告警、局面保持。

## 元数据：声明即契约,宿主机器强制

`contract.Meta` 逐轴双向声明(见上表),宿主注册时逐轴求差;除注册校验外,
`Host` 提供**元数据查询面**:

- `Dependents(id)`——谁在依赖它(卸前判断/控制面链路展示);
- `CapabilityIndex()`——能力词 → 声明者(装配自检"谁提供某能力");
- `Functions()`——函数目录快照(名字+Schema+属主)。

## 通用库开箱面(零装配可用)

- **默认装配件**:`Options` 零值即可用——Events=内存总线 `MemEvents`、
  Settings=`MapSettings`(线程安全,Set/Get/Keys)、Points=`MemPoints`、
  Hooks=内存 hook 总线 `MemHooks`;全部可装配注入替换;
- **默认事件总线**:订阅/广播/主题位开箱可用,业务总线可装配注入替换;
- **Hook 派发**:`OnHook` 注册、`TriggerHook` 触发,回调返回带上下文的 `HookResult`
  (Block/Reason/Data) 或**直接返回数据**(框架归一,见 `contract.HookResultOf`);
  精确表达"阻断 + 产出";插件注册的订阅与 hook 回调在卸载/热替换时
  由宿主**自动回滚**(每插件一个 effect 撤销栈);
- **配置生效事件**:`SetConfig` 成功经总线广播 `plugin.config.updated`
  (payload `{"plugin":id,"config":cfg}`),允许观察者无侵入联动;
- **统一关停**:`Host.Close(ctx)` 逆注册序移除全部插件并对 `contract.Disposer`
  (wasm 实例/远程会话)逐一清退,错误聚合(尽力而为不中断);
- **控制面快照**:`Host.Snapshot()` 输出 `{plugins,functions,capabilities,
  applied_config}` 全景 JSON(元数据/函数目录/能力索引/生效配置);
- **生命周期简日志**:`Options.Logf` 注入注册/替换/卸载/配置日志;
- **guest 日志直出**:`wasm.Options.LogFn` 接 ABI 的 `log(level,msg)` 注入
  (缺省静默)——插件内部日志直入宿主日志面;
- `loader.HostFace` 统一宿主面:非 core 宿主包一层桥(参照消费仓的 plugbridge)
  即接入全部装载器,业务仓零加载代码。

最小开箱示例见 `examples/inproc`(注册/调用/配置/事件/快照/关停全链路)。

## ctx 能力面（contract.Surface）

注册时宿主注入按 `Meta.Capabilities` 裁剪的 Surface 视图：

- `Call`（`plugin.call`）——跨插件函数调用;
- `Plugins`/`GetPlugin`（`plugin.meta`）——读插件目录与其它插件元数据;
- `GetConfig`（`config.read`）/ `SetConfig`（`config.write`）——跨插件
  配置读写,受 `ConfigFieldSpec.Readable/Writable` 字段级标志二次裁剪(egop 始终可读写);
- `PublishEvent/SubscribeEvent`（`event.emit`/`event.listen`）——事件广播/
  订阅;订阅回调带 `(ctx, topic, e Event)` 并返回撤销函数;
- `Persist`（`storage.persist`）/ `KV`（`storage.kv`）——插件专属存储(后端经
  `Options.Storage` 注入);
- `Exec`（`exec.cmd`）——执行命令(注入 `ExecFn`);
- `Net`（`net.access`）——出站网络(HTTP/HTTPS/SSE/WebSocket/WebTransport 等,经 `Options.Net` 注入,
  egop 只定义最小面与数据结构、不实现传输;且出站目标须是网络协议,`file://` 等本地访问被拒);
- `FS`（`fs.read`/`fs.write`）——全局文件系统受控视图(经 `Options.FS` 注入;范围/沙箱
  策略由实现决定,读写按声明分向门控;区别于 `storage.persist` 的插件专属隔离目录);
- `Op(ctx, name, input)`（扩展能力,核心零业务）——wire 短名经
  `Options.OpAliases` 映射到守卫能力词再查处理器;装配层注入 `Ops`。

`Host.SurfaceFor(pluginID)` 导出同一视图——远程插件的能力回程（HostCall 帧）
一律经它路由,与进程内插件同语义（未声明即拒绝）。

## 与自有宿主集成(HostFace 桥)

本库既可独立使用(`host.Host[C]` 即宿主),也可接进**已有宿主**:让既有宿主
包装一层 `loader.HostFace`(Register/Replace/Remove/HasPlugin/Plugins/
SetConfig/AppliedConfig/SurfaceFor/Call 九法),即可免费获得 wasm 目录/热更/
远程通道双方向全部装载能力,无需改既有宿主内核——消费方只需写类型桥
(JSON 转译 + typed 工具包装),如同消费仓的 plugbridge 所为。

## 跨平台与浏览器(I/O 注入面)

核心(`contract`/`host`/`schema`/`undo`/`exchange`/`loader`)的默认路径零 os/net
依赖,已在 `GOOS=js GOARCH=wasm` 与 `wasip1` 下通过编译;库自身的外部 I/O 全走
**可注入缝**,浏览器/内嵌消费方无需真实文件系统或网络:

- **读插件文件**:`wasm.LoadFS(ctx, data, name, opts)` 直接吃字节;目录发现用
  `wasm.ScanFS(ctx, fsys, opts)`,`mount.Sources.FS` / `autoload.Options.FS`
  注入任意 `io/fs.FS`(如 `fstest.MapFS` 或自定义实现)后,`Dirs` 即视作该 FS
  内的根目录——OS 目录路径只是 `os.DirFS` 的便捷默认。
- **网络**:插件↔框架的远程通道传输经 `Sources.StreamDial/StreamAccept` 注入(egop 只收发
  字节帧、不建连接);插件**出站网络**(调 LLM API 等)经 `host.Options.Net` 注入——egop
  只定义 `Net.Request`(单向请求族)/`Net.DialStream`(双向消息流族)最小面,具体传输由
  装配层实现(桌面 `net/http`+websocket、浏览器 wasm `fetch`/`WebSocket`/`WebTransport`)。
  出站目标须是网络协议(内置 `http/https/ws/wss`,可 `Options.NetSchemes` 补充),
  `file://` 等本地/特殊 scheme 在转交装配实现前即被协议门拒绝。
  远程通道与 wasm 目录装载属桌面侧能力:浏览器消费方不注入 `Sources.StreamDial/StreamAccept`、
  不调 `ScanDir` 即不会触网/触盘。
- **持久化**:插件专属存储后端经 `host.Options.Storage`(实现 `contract.Storage`,**必填**)
  注入——egop 不内置任何文件/网络能力,未注入时 `Surface.Persist()`/`KV()` 返回不可用。

## 命令与维护

```sh
make hygiene            # fmt + vet + 全量测试(修改后必跑)
go test ./...           # 全量(loaders 含真 wasm 实例 e2e)
```

- 修改 WASM ABI 后:同步改 `loader/wasm/testdata/demo.wat` → 用 wabt 的
  wat2wasm 重编 `demo.wasm`(fixtures 双双入库),再跑全量;
- 工具在 loader 侧**无类型**:`ToolRaw(name)(func(ctx,tctxJSON,args)
  (string,error),bool)`,typed 包装永远留在消费方装配层(tctx 即线上 JSON)。
- 参考文档见 [`doc/`](doc/README.md)：契约词汇、API、用法指南。

## 边界

- 远程通道不建连接、无内置加密:传输与 mTLS 由外部注入实现(本库只收发帧);
- 无自动重连:断线 = 卸载 + 告警（重连 = 重新注册）;
- 热更采用轮询(默认 1s):零额外依赖、跨平台确定,两段确认天然抗半截写入;
- 远程通道与 wasm 目录装载是桌面侧能力;浏览器(js/wasm)经 `io/fs.FS`/`LoadFS`
  字节接口 + 进程内注册使用核心面,持久化(Storage)不注入即不可用。