# 用法指南

可跑示例见 `examples/`（`inproc` / `dependency` / `lazy` / `wasm` / `rawconn` / `net` /
`exchange` / `storage` / `events` / `controlplane`），每目录有 README。这里是按主题的最小片段。

## 1. 进程内注册 + 调用

```go
type hello struct{}
func (hello) Meta() contract.Meta {
    return contract.Meta{ID: "demo.hello", Name: "Hello", Version: "1",
        Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "greet"}}}}
}
func (hello) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
    return json.RawMessage(`"hi"`), nil
}

h := host.New[any](host.Options[any]{Logf: log.Printf})   // 零值 Options 亦可用
_ = h.Register(hello{})
out, _ := h.Call(context.Background(), "demo.hello", "greet", json.RawMessage(`{}`))
defer h.Close(context.Background())
```

## 2. 依赖 + 跨插件调用

- 依赖方在 `Meta.Requires.Deps` 声明 `kind:"init"` 的依赖；被依赖方须先注册（或由
  `RegisterMany` 自动排序）。
- 跨插件调用经 `Surface.Call`，条件是依赖方声明了 `plugin.call` 能力。

```go
// B 依赖并调用 A
type sigma struct{ surface contract.Surface }
func (s *sigma) Meta() contract.Meta {
    return contract.Meta{
        ID: "calc.sigma", Name: "Sum", Version: "1",
        Provides: contract.Provides{                          // 供给面
            Capabilities: []string{contract.CapCallPlugins},  // 声明才可调
            Functions:    []contract.FuncSpec{{Name: "sum"}},
        },
        Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "calc.basic", Kind: contract.DepInit}}},
    }
}
func (s *sigma) SetSurface(e contract.Surface) { s.surface = e }                  // 拿门控视图
func (s *sigma) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
    // 内部跨插件调用 calc.basic.add
    return s.surface.Call(ctx, "calc.basic", "add", json.RawMessage(`{"a":1,"b":2}`))
}
```

被调函数经 `contract.OriginFrom(ctx)` 读调用者（`Origin{ID,Version,Kind:"call",Point}`）；
宿主/应用直接 `Host.Call` 时为 `Origin{Kind:"host"}`（始终有显式来源，无需判 nil）。

## 3. 批量装载 + 自动排序

不想管顺序时，把一批插件交给 `RegisterMany`，框架按 `Requires`（DepInit）拓扑
排序后依序注册，缺依赖/成环/重复 id 的件**单独失败**、不阻断其余：

```go
rep := h.RegisterMany([]contract.Plugin{b, c, a})   // 乱序传入
// rep.Registered -> ["a","b","c"] 顶排序; rep.Failed -> 失败件及原因
```

依赖**后到**时可用 `RegisterLazy`：依赖未满足先入待补载队列（返回
`StatusPending`），等后续 `Register`/`RegisterMany`/`Replace` 使依赖到位后自动补载
（含传递链）。重复懒登记同 id 会**更新**待补载实现（非错误）；依赖已满足时懒登记立即注册
（返回 `StatusRegistered`）。`RegisterMany` 里依赖待补载插件的条目会进 `rep.Pending`
（而非 `Failed`），随依赖到位整链补载。

## 4. 配置下发 + Schema 校验

配置只在 `Meta.Provides.Config` 声明字段后才会被接受；`SetConfig` 用各自 `Schema` 校验。

```go
type cfg struct{}
func (cfg) Meta() contract.Meta {
    return contract.Meta{ID: "demo.cfg", Name: "C", Version: "1",
        Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "level", Schema: json.RawMessage(`{"type":"integer"}`)}}}}
}
func (cfg) ApplyConfig(c json.RawMessage) error { return nil }   // 实现 Configurable 才能接到

_ = h.SetConfig("demo.cfg", json.RawMessage(`{"level":"high"}`)) // 拒绝: 非 integer
_ = h.SetConfig("demo.cfg", json.RawMessage(`{"level":3}`))      // 接受
```

`SetConfig` 成功后经默认事件总线广播 `plugin.config.updated`
（payload `{"plugin":id,"config":cfg}`）。

**写语义**：`host.SetConfig(id, cfg)` 是**整对象替换**（每次覆盖整份生效配置）；
`host.SetConfigField(id, key, value)` 是**合并单字段**（读旧值→补 key→整对象下发）——
web 配置界面按字段保存用后者，全量表单提交用前者。`Surface.SetConfig(id, key, value)`
是带能力门控的单字段合并。

**读语义（权威读回）**：插件实现 `contract.ConfigProvider`（`Config() json.RawMessage`）时，
`host.EffectiveConfig(id)` 优先读它（覆盖默认值补齐/归一化/脱敏后的真值），未实现则回退
`host.applied`（宿主推过的原始 delta）;`host.GetConfig(id, key)`/`Surface.GetConfig` 读的就是
`EffectiveConfig` 的对应字段。配置声明里 `ConfigFieldSpec.Default` 是**声明级默认值**（供 UI
展示/回填），运行时真实默认以插件的 `Config()` 为准。

```go
// 插件实现 Config 读回其当前生效配置(web 界面拿它显示真实状态;可对 secret 脱敏)
type cfg struct{ level int }
func (c *cfg) ApplyConfig(raw json.RawMessage) error { _ = json.Unmarshal(raw, &struct{ Level *int `json:"level"` }{&c.level}); return nil }
func (c *cfg) Config() json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"level":%d}`, c.level)) }

got, _ := h.EffectiveConfig("demo.cfg")          // 优先 Config();未实现回退 applied
_ = h.SetConfigField("demo.cfg", "level", json.RawMessage(`5`)) // 合并,保留其它字段
```

**跨插件配置读写权限**：`ConfigFieldSpec` 里的 `Readable`/`Writable` 控制「别的插件
能不能读/写这个字段」（egop 始终可读写），配合 `config.read`/`config.write` 能力词——
两层都满足才放行。

```go
// 目标声明:apikey 只写(别的插件能覆盖、读不回)、rule 只读
Provides: contract.Provides{
    Config: []contract.ConfigFieldSpec{
        {Key: "apikey", Writable: true, Readable: false},
        {Key: "rule",   Writable: false, Readable: true},
    },
}

// 调用方:声明 config.write 才能写,声明 config.read 才能读
sur.SetConfig("target.id", "apikey", json.RawMessage(`"new-key"`)) // 需 config.write + 字段 writable
v, _ := sur.GetConfig("target.id", "rule")                        // 需 config.read + 字段 readable
```

## 5. 事件（能力门控）

发布/订阅都要求先声明能力：

```go
// 声明 event.emit 才能发布，event.listen 才能订阅
func (p *p) CallFunc(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
    p.surface.PublishEvent(ctx, "dyn.mine.topic", json.RawMessage(`{"x":1}`))   // 便捷面:无标签
    // 或发布带标签的完整事件(Type 即主题,**必填**;Source/Version 框架回填):
    p.surface.Publish(ctx, contract.Event{Type: "x.sent", Labels: map[string]string{"sev": "high"}})
    return nil, nil
}

// 精确主题订阅(等价于 EventFilter{Type:"topic"})
unsub := surface.SubscribeEvent("topic", func(ctx context.Context, topic string, e contract.Event) { ... })
// 过滤订阅:主题通配 + 标签匹配,任一/多个字段组合,命中才回调
surface.SubscribeEventFilter(&contract.EventFilter{Type: "x.*", Labels: map[string]string{"sev": "high"}},
    func(ctx context.Context, topic string, e contract.Event) { ... })
// nil(或零值)过滤 = 匹配所有事件
surface.SubscribeEventFilter(nil, func(ctx context.Context, topic string, e contract.Event) { ... })
```

事件是一份**固定结构** `contract.Event{Type, SubType, Labels, Payload}`——发布者给
`Type`(=主题,**必填**,空则丢弃)/`SubType`/`Labels`(自由键值)/`Payload`,框架回填
`Version` 与 `Source`(来源即 `contract.Origin`,与 hook/调用的来源同构)。

订阅按 `contract.EventFilter` 匹配（`nil`/零值 = 全部；字段间 AND，可只设一个或多个）：

| 字段 | 语义 |
|---|---|
| `Type` | 主题;含 `*` 通配(`*` 匹配任意字符序列,如 `chat.*`) |
| `SubType` | 子类型精确 |
| `SourceID` | 来源插件 id（`Origin.ID`） |
| `SourceVersion` | 来源插件版本（`Origin.Version`） |
| `SourceKind` | 来源类别(`Origin.Kind`:event/hook/call/host) |
| `Labels` | 要求事件带这些键值(子集相等) |

`Origin.Point`(事件即 Type)与 `Origin.At`(时间戳)不适合作等值键,不参与过滤。
`Payload` 是不透明 JSON，不参与过滤（保持内容无关,不做深层解析）。订阅回调收到的
`e.Source`（`*contract.Origin`）带来源上下文：`ID`/`Version`/`Kind`/`Point`/`At`;宿主级
事件 `Kind=host`。

**投递约定**：回调拿到的事件是**多个订阅者共享的只读值**（`Labels` 是 map、`Source`
是指针，均为引用），订阅者不得改写。三端回调签名统一为 `(ctx, topic, e)`——
远程插件的 `ctx` 是本侧会话上下文（`ctx` 不跨边界），wasm 因 ABI 边界无 ctx。

## 6. 函数 schema 校验（默认开）

声明了 `FuncSpec.Input/Output` 的函数，`Host.Call` 会自动校验入参/返回（入参
调用前拒、返回调用后拒）；`Options.DisableFuncValidation` 全局关闭。

```go
Input:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}`),
Output: json.RawMessage(`{"type":"integer"}`),
// 支持 type 数组与 anyOf: {"type":["integer","string"]} / {"anyOf":[...]}
```

## 7. WASM 加载

三种给字节的方式：

```go
// 直接给内容(浏览器/内嵌)
p, err := wasm.LoadFS(ctx, bytes, "demo.egop.wasm", wasm.Options{})

// 目录递归发现(fs.FS 注入缝)
plugs, errs := wasm.ScanFS(ctx, fstest.MapFS{"plugins/demo.egop.wasm": {Data: bytes}}, wasm.Options{})

// OS 目录
plugs, errs = wasm.ScanDir(ctx, "./plugins", wasm.Options{})
```

形态：`*.egop.wasm`（自定义段 `egop.manifest` 或 `egop_meta` 导出）与 `*.egop.zip`
（`manifest.json` + `plugin.wasm` + `assets/`）。

## 8. 远程插件（传输无关，两个方向）

远程通道不建连接：传输（http/https/websocket/任意字节流）由外部注入，egop 只在
一条 `remote.Stream` 上收发 JSON 帧。一站式装配（框架侧）：

```go
rt, warns, err := mount.Mount(ctx, hf, mount.Sources{
    Dirs:   []string{"./plugins"},            // wasm 目录(可选 Watch 热更)
    Remote: []mount.RemoteSpec{{ID: "x", Addr: "custom://x"}},  // 框架主动拨出
    // 出/入站传输注入(与 io/fs.FS 同理;不注入则对应方向不启用):
    StreamDial:   func(ctx context.Context, addr string) (remote.Stream, error) { ... },
    StreamAccept: func(ctx context.Context) (remote.Stream, error) { ... }, // 插件主动拨入
})
defer rt.Close()
```

插件作者侧（入站，先建好传输再交一条 Stream）：

```go
sess, err := remote.AttachStream(ctx, stream, mf, &remote.PluginOps{
    CallFunc: func(ctx, fname, input json.RawMessage) (json.RawMessage, error) { ... },
    Tool:     func(ctx, tool string, args, tctx json.RawMessage) (json.RawMessage, error) { ... },
})
<-sess.Done()
```

`remote.BindStream(ctx, rw)` 把任意 `io.ReadWriteCloser` 包成 `remote.Stream`；框架侧
`ServeStream`/`DialStream`、插件侧 `ServePluginStream`/`AttachStream` 覆盖两个
连接方向，同一条 JSON 帧表。

## 9. 热更（autoload / mount Watch）

```go
rt, _, _ := mount.Mount(ctx, hf, mount.Sources{Dirs: []string{"./plugins"}, Watch: true})
for e := range rt.Events() { /* register / replace / remove / failed */ }
```

语义：内容 hash 两段确认（抗半截写）、**删除同样两段确认**（连续两轮未见才卸载，
抗目录瞬态读失败误卸）、替换失败回退保旧版（`Replace` 与 `Register` 同款契约校验，
拒换即旧版继续服务）、替换成功重放配置、坏包隔离、删除被依赖者 fail-closed
（点名与槽位依赖同判）；`mount` 首装"拍至稳定"补全乱序依赖链，装配失败时句柄
自行**全清**（含反注册目录阶段已进册的插件）。

## 10. ctx 能力面（Surface）对照

| 能力词 | Surface 方法 | 未声明时 |
|---|---|---|
| `plugin.call` | `Call` | 报错 |
| `plugin.meta` | `Plugins` / `GetPlugin` | 返回空 / `false` |
| `config.read` | `GetConfig` | 返回 `(nil,false)` |
| `config.write` | `SetConfig` | 报错 |
| `event.emit` | `PublishEvent` / `Publish` | no-op |
| `event.listen` | `SubscribeEvent` / `SubscribeEventFilter` | no-op（空撤销函数） |
| `storage.persist` | `Persist` | 返回 `(nil,false)` |
| `storage.kv` | `KV` | 返回 `(nil,false)` |
| `exec.cmd` | `Exec` | 报错 |
| `net.access` | `Net`（Request / DialStream，协议门：拒绝 `file://` 等非网络 scheme） | 返回 `(nil,false)` |
| `fs.read` | `FS().ReadFile`（全局文件系统受控视图，范围/沙箱由注入实现决定） | `FS()` 返回 `(nil,false)`；只声明 write 时 ReadFile 报错 |
| `fs.write` | `FS().WriteFile` | 同上（分向门控） |
| 扩展能力（装配注入） | `Op(ctx,name,input)` | 经 `OpAliases` 映射守卫词后判定 |

`Host.SurfaceFor(pluginID)` 可导出同一门控视图（远程插件的能力回程 `HostCall`
一律经它路由，与进程内插件同语义）。`Persist`/`KV` 的后端经 `Options.Storage` **必填注入**、
`Exec` 经 `Options.ExecFn` 注入、出站网络（HTTP/HTTPS/SSE/WebSocket/WebTransport 等）经
`Options.Net` 注入、全局文件系统经 `Options.FS` 注入——egop 自身不实现任何文件/网络/
命令执行传输。出站目标须是网络协议（内置 `http`/`https`/`ws`/`wss`，`Options.NetSchemes`
可补充），`file://` 等本地访问被协议门拒绝。

## 11. Hook 派发与 effect 自动回滚

hook 是"注册回调 → 触发 → 回调返回阻断/产出"的机制。回调可返回带上下文的
`HookResult`（`Block`+`Reason`+`Data`），也可**直接返回数据**——框架经
`contract.HookResultOf` 归一（直接数据 → `HookResult{Data: <该值的 JSON>}`）。
阻断靠 `Block` 字段，不靠返回 nil/false 猜。

```go
// 直接返回数据(egop 归一成 HookResult{Data:...})
_ = h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
    return map[string]any{"normalized": true}
})
// 返回完整 HookResult(阻断 + 理由 + 产出)
_ = h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
    return contract.HookResult{Block: true, Reason: "denied", Data: json.RawMessage(`{"policy":"deny-all"}`)}
})
for _, r := range h.TriggerHook(context.Background(), "demo.validate", json.RawMessage(`{"n":1}`)) {
    if r.Block { /* 阻断后续 */ }
}
```

触发结果里 `r.Origin` 是统一来源（`Origin{ID: 注册者插件, Kind:"hook", Point:hookID, At}`），
`r.Seq` 是回调顺序——与事件来源（`Event.Source`）、调用来源（`OriginFrom(ctx)`）同构。

插件侧也可注册（经 `Surface.OnHook`），且**无需自查撤销**——插件卸载/热替换时
宿主会自动回滚它注册的 hook 回调与事件订阅（每插件一个 effect 撤销栈）。