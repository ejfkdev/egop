# API 参考

按包列出公开面。签名以当前源码为准；详情见各包 doc comment。

## contract（词汇真源）

**数据类型**

```go
type Meta struct { ID, Name, Version, Description; Homepage, License string;
    Authors, Tags []string; Provides Provides; Requires Requires; Slot string;
    Extensions map[string]json.RawMessage }  // Extensions=自由扩展键值(开发者自约定,egop 不解释;键勿用 "egop." 保留前缀)
type Provides struct { Points, Capabilities []string; Hooks []HookPointSpec;
    Events []EventTopicSpec; Functions []FuncSpec; Config []ConfigFieldSpec }
type Requires struct { Listens []string; Deps []Dependency; Tools []string }
type Manifest struct { Meta; Tools []FuncSpec }
type SlotSpec struct { ID, Doc; Provides, Hooks, Events, Functions, Capabilities, Config,
    Listens, NeedsTools, Needs []string; Builtin bool; Extensions map[string]json.RawMessage }
type FuncSpec struct { Name, Description string; Input, Output json.RawMessage;
    Extensions map[string]json.RawMessage }  // Extensions=自由扩展键值(与 Meta.Extensions 同构)
type HookPointSpec struct { ID string; Kind HookKind; Description string; Payload, Result json.RawMessage;
    Extensions map[string]json.RawMessage }  // Kind=observe 的点回调 Block 无效(宿主按声明丢弃)
type EventTopicSpec struct { ID, Description string; Payload json.RawMessage; Extensions map[string]json.RawMessage }
type ConfigFieldSpec struct { Key, Description string; Schema, Default json.RawMessage;
    Readable, Writable, Secret bool; Extensions map[string]json.RawMessage }  // Secret=声明优先脱敏(观察事件)
type Dependency struct { Plugin, Slot string; Kind DependencyKind; MinVersion string;
    Extensions map[string]json.RawMessage }  // Plugin/Slot 恰有其一(双空拒载;双取 Slot 优先)
type ResultEnvelope struct { OK bool; Result json.RawMessage; ResultB64, Error, Type string; At int64; Meta json.RawMessage }
type Event struct { Type, SubType string; Version int; Source *Origin; Payload json.RawMessage; Labels map[string]string }
type EventFilter struct { Type, SubType string; SourceID, SourceVersion string; SourceKind OriginKind; Labels map[string]string }
func (f EventFilter) Match(e Event) bool   // nil/零值命中一切;Type 含 '*' 通配;Labels 子集相等;字段间 AND
type Net interface { Request(ctx, Request) (*Response, error); DialStream(ctx, url string, headers map[string]string) (Stream, error) }
type Request struct { Method, URL string; Headers map[string]string; Body io.Reader }
type Response struct { Status int; Headers, Trailers map[string]string; Body io.Reader }
type Stream interface { Send([]byte) error; Recv() ([]byte, error); Context() context.Context }
type FileStore interface { Read(name string) ([]byte, error); Write(name string, data []byte) error;
    Append(name string, data []byte) error; List() ([]string, error) }  // 插件专属文件(Write=整文件覆盖,Append=末尾追加)
type KeyValue interface { Get(key string) ([]byte, bool); Put(key string, v []byte); Delete(key string); Keys() []string }
type FS interface { ReadFile(name string) ([]byte, error); WriteFile(name string, data []byte) error;
    ReadDir(name string) ([]DirEntry, error) }  // 全局文件系统注入后端(范围/沙箱由实现定;ReadDir 列一层)
type DirEntry struct { Name string; IsDir bool; Size, ModTime int64 }  // ModTime=unix 毫秒,0 未知
type Storage interface { File(pluginID string) FileStore; KV(pluginID string) KeyValue }
type OriginKind string   // event | hook | call | host
type Origin struct { ID, Version string; Kind OriginKind; Point string; At int64 }
```

**插件接口族**

```go
type Plugin interface { Meta() Meta }                                   // 必须
type FunctionProvider interface { CallFunc(ctx, fname, input) (json.RawMessage, error) }
type ToolProvider[C any] interface { ToolSpecs() []FuncSpec; Tool(name) (ToolFunc[C], bool) }
type ToolFunc[C any] func(ctx, tctx *C, input json.RawMessage) (json.RawMessage, error)
type HookFunc func(ctx, hookID string, data json.RawMessage) any  // 返回 HookResult 或直接数据,框架归一
type HookResult struct { Block bool; Reason string; Data json.RawMessage; Origin *Origin; Seq int } // Block/Reason/Data 回调写;Origin/Seq 框架填
func HookResultOf(v any) HookResult  // 归一:直接数据(nil/RawMessage/[]byte/string/值)→HookResult{Data};HookResult 原样
type Configurable interface { ApplyConfig(cfg json.RawMessage) error }
type ConfigProvider interface { Config() json.RawMessage }  // 权威读回(默认/归一/脱敏)
type SurfaceAware interface { SetSurface(e Surface) }
type Disposer interface { Close(ctx) error }
```

**Surface 能力面**（`contract.Surface`）

```go
Plugins() []Meta
GetPlugin(id string) (Meta, bool)    // 读某插件元数据(以上两者均受 plugin.meta 门控)
Call(ctx, pluginID, fname string, input json.RawMessage) (json.RawMessage, error)
GetSetting(key string) (json.RawMessage, bool)
PublishEvent(ctx, topic string, payload json.RawMessage)
Publish(ctx, e Event)                // 完整事件(Type/SubType/Labels/Payload;Source/Version 框架回填)
SubscribeEvent(topic string, fn func(ctx, topic string, e Event)) func()
SubscribeEventFilter(f *EventFilter, fn func(ctx, topic string, e Event)) func()  // nil/零值 = 全部
OnHook(hookID string, fn HookFunc) func()
Persist() (FileStore, bool)
KV() (KeyValue, bool)
Net() (Net, bool)
FS() (FS, bool)                    // 全局文件系统(fs.read/fs.write 分向门控;装配注入 Options.FS)
Exec(ctx, cmd string) (string, error)
Op(ctx, name string, input json.RawMessage) (json.RawMessage, error)
GetConfig(pluginID, key string) (json.RawMessage, bool)   // 跨插件读配置字段(config.read + 字段 readable)
SetConfig(pluginID, key string, value json.RawMessage) error // 跨插件写配置字段(config.write + 字段 writable)
```

**配置字段**（`ConfigFieldSpec`）：`Key` + `Schema` + `Readable`(其它插件可读) +
`Writable`(其它插件可写)；egop/宿主始终可读写。见 contract 能力词
`CapConfigRead`/`CapConfigWrite`。

**辅助**

```go
func HasCapability(m Meta, cap string) bool
func PointID(pluginID, pointID string) string   // "dyn.<plugin>.<point>"
func EventID(pluginID, short string) string
const DynamicPrefix = "dyn."
const ReservedExtPrefix = "egop."               // Extensions 键保留前缀(egop 自身特性;自定义键勿用)
const EventConfigUpdated = "plugin.config.updated"  // SetConfig 成功的观察事件主题
const EventPluginRegistered = "plugin.registered"   // 插件注册(Register 成功后广播)
const EventPluginRemoved    = "plugin.removed"      // 插件卸载(含级联 victim;软依赖方订阅降级)
const EventPluginReplaced   = "plugin.replaced"     // 插件热替换(Replace 成功后广播)
func IsFrameworkTopic(topic string) bool        // 框架保留主题判定(插件经 Surface 发布被拒)
func Ext[T any](ext map[string]json.RawMessage, key string) (T, bool)  // Extensions 取值+解码(缺键/失败=零值,false)
func CloneMeta(m Meta) Meta                      // 深拷贝(注册快照不变量:宿主入册即冻结)
func WithOrigin(ctx context.Context, o *Origin) context.Context  // 注入调用来源(框架用)
func OriginFrom(ctx context.Context) *Origin      // 被调函数读调用者(nil = 宿主/应用发起)
```

## loader（统一宿主面）

```go
type HostFace interface {
    Register(p contract.Plugin) error
    Replace(p contract.Plugin) error
    Remove(pluginID string, cascade bool) ([]string, error)
    HasPlugin(id string) bool
    Plugins() []contract.Meta
    SetConfig(pluginID string, cfg json.RawMessage) error
    AppliedConfig(pluginID string) (json.RawMessage, bool)
    SurfaceFor(pluginID string) (contract.Surface, bool)
    Call(ctx, pluginID, fname string, input json.RawMessage) (json.RawMessage, error)
}
```

非 core 宿主包一层桥实现这 9 个方法，即接入全部装载器（wasm 目录/热更/远程通道）。

## host（泛化宿主 `Host[C]`）

```go
func New[C any](opts Options[C]) *Host[C]   // 零值 Options 亦可用

type Options[C any] struct {
    Points  Points; Events Events; Settings Source
    Hooks   Hooks              // Hook 派发后端(默认 MemHooks)
    Storage contract.Storage   // 持久化注入后端(必填;未注入 → Persist/KV 不可用)
    Net     contract.Net         // 出站网络注入后端(必填;未注入 → Net 不可用)
    NetSchemes []string          // 补充允许的网络 scheme;内置 http/https/ws/wss 恒定;/file:// 等非网络一律拒
    FS      contract.FS          // 全局文件系统注入后端(nil → Surface.FS 不可用;fs.read/fs.write 分向门控)
    ExecFn  func(ctx, cmd) (string, error)
    Ops     map[string]Op            // 扩展能力: 能力词 → 处理器
    OpAliases map[string]string      // wire 短名 → 守卫能力词
    ToolNames func() []string        // 框架已就位的工具面
    SlotLookup func(id) (contract.SlotSpec, bool)
    DisableFuncValidation bool       // 关函数 schema 校验(默认 false=开)
    Logf func(format string, args ...any)   // 生命周期日志
}
type Op func(ctx, input json.RawMessage) (json.RawMessage, error)
type Source = contract.Source
```

**方法**

```go
Register(p contract.Plugin) error                              // 单件注册(顺序敏感;Meta 入册即 CloneMeta 快照)
RegisterMany(plugs []contract.Plugin) RegisterReport           // 批量+拓扑排序+隔离
RegisterLazy(p contract.Plugin) (RegisterStatus, error)         // 依赖未满足先入队,到位后自动补载
// type RegisterStatus int // StatusRegistered | StatusPending
Replace(p contract.Plugin) error                                 // 与 Register 同款契约校验(依赖/槽位),拒换保旧版;快照+配置回灌
Remove(pluginID string, cascade bool) ([]string, error)          // 级联卸载 / fail-closed(点名与槽位依赖同判;victims 去重)
Call(ctx, pluginID, fname string, input json.RawMessage) (json.RawMessage, error) // 含 schema 校验
SetConfig(pluginID string, cfg json.RawMessage) error          // 校验 + 广播 config.updated(整对象替换;观察事件 Secret 声明优先脱敏)
SetConfigField(pluginID, key string, value json.RawMessage) error // 单字段合并(再下发)
SurfaceFor(pluginID string) (contract.Surface, bool)
Close(ctx) error                                               // 逆注册序 + Disposer 清退
Plugins() []contract.Meta
HasPlugin(id string) bool
Dependents(pluginID string) []string                           // 谁在依赖它
CapabilityIndex() map[string][]string                          // 能力词 → 声明者
Functions() []FnView                                            // 函数目录快照
Tools() []Tool[C]                                               // 工具收集(声明 tool.provide)
OnHook(hookID string, fn contract.HookFunc) func()             // 注册 hook 回调(返回撤销)
TriggerHook(ctx, hookID string, data json.RawMessage) []contract.HookResult // 触发 hook;observe 点按声明丢弃 Block
AppliedConfig(pluginID string) (json.RawMessage, bool)          // 宿主推过的缓存
EffectiveConfig(pluginID string) (json.RawMessage, bool)        // 权威读回:ConfigProvider 优先,回退 applied
GetConfig(pluginID, key string) (json.RawMessage, bool)         // 读 EffectiveConfig 的单个字段
Snapshot() Snapshot                                            // {plugins,functions,capabilities,applied_config}
```

注册期校验(注册/替换口共用,fail-closed):插件 id 与函数名拒绝空白/控制字符,
**函数名另拒 `"."**(函数目录键 `id.fn` 的分隔符);依赖项 `Plugin`/`Slot` 双空拒载;
插件经 Surface 发布框架保留主题(`plugin.*` 生命周期/`plugin.config.updated`)被拒发并留痕。

```go
type RegisterReport struct { Registered []string; Pending []string; Failed []RegisterFailure }
type RegisterFailure struct { ID string; Err error }
type Tool[C any] struct{ /* Spec contract.FuncSpec; Run(ctx, tc *C, args) (string,error) */ }
type FnView struct { PluginID string; Spec contract.FuncSpec }
type Snapshot struct { Plugins []contract.Meta; Functions []FnView; Capabilities map[string][]string; Applied map[string]json.RawMessage }
```

## loader/wasm

```go
const DefaultMaxPages uint32 = 1024
const DefaultMaxEntryBytes int64 = 256 << 20   // zip 单条目解压上限
const DefaultMaxTotalBytes int64 = 1 << 30     // zip 整包聚合解压上限
type Options struct {
    MaxMemoryPages uint32
    LogFn func(level, msg string)
    ExtraSuffixes []string   // 追加 zip 包后缀(如品牌 ".x.zip";布局同 .egop.zip;库内不内置业务词)
    MaxEntryBytes int64      // 0 = DefaultMaxEntryBytes(防 zip bomb)
    MaxTotalBytes int64      // 0 = DefaultMaxTotalBytes
}

func IsPluginFile(name string, extra []string) bool               // 后缀判定唯一收敛点(扫描/装载共用)
func LoadFile(ctx, path string, opts Options) (*Plugin, error)      // 单文件(os)
func LoadFS(ctx, data []byte, name string, opts Options) (*Plugin, error)  // 直接字节
func ScanFS(ctx, fsys fs.FS, opts Options) ([]*Plugin, []error)     // fs.FS 遍历
func ScanDir(ctx, dir string, opts Options) ([]*Plugin, []error)    // = ScanFS(os.DirFS(dir))
const MaxFSReadBytes = 8 << 20   // fs_read 单次回传 guest 的 ABI 尺寸护栏(信封 base64 放大前拒)

type Plugin struct{ /* Meta/CallFunc/ApplyConfig/Config/SetSurface/ToolSpecs/ToolRaw/Close */ }
func (p *Plugin) ToolRaw(name) (func(ctx, tctxJSON, args json.RawMessage) (string, error), bool)
func (p *Plugin) Assets() map[string][]byte   // zip 内 assets/ 静态资源表副本(裸 wasm = 空表)
```

zip 包缺 `plugin.wasm` = **无代码插件**(纯清单/资产):可加载、可注册,声明
函数/工具/配置/hook 点等需代码兑现的面即拒载;`CallFunc`/`ApplyConfig` 返回
干净错误(非 panic)。

实例语义(机制内建,见 contract.md §8"实例池与自愈"):

- **实例池**:清单扩展 `egop.pool` 声明并发池(缺省 1、上限 4,zip 与裸 wasm
  都生效);入口 TryLock 空闲实例,同调用栈嵌套(经 ctx 重入标记识别)池耗尽
  立即返回 busy;事件/hook 观察面钉主实例(不随池放大)。
- **活体工具面**:`ToolSpecs` 优先调 guest 的 `egop_tool_specs` 导出(动态工具
  如 MCP 运行期发现),无导出/失败/断后回落 `Manifest.Tools` 静态清单。
- **revive**:调用被 ctx 取消/trap 打断的实例,下次调用自动重建(复用编译产物、
  重放 `egop_init` 与最近配置);显式 `Close` 是终态不复活。
- **系统时钟**:`_initialize`/`_start` 之外,实例化接 WithSysWalltime/Nanotime/
  Nanosleep(guest 的 time.Now/Sleep 是真钟——wazero 缺省是假钟桩)。

## loader/remote

远程通道不再建连接：传输注入，egop 只在 `remote.Stream` 上收发 JSON 帧。

```go
type RemoteHost = loader.HostFace
type Stream interface { Send([]byte) error; Recv() ([]byte, error); Context() context.Context; Close() error }

func BindStream(ctx context.Context, rw io.ReadWriteCloser) Stream   // 长度前缀帧化
func AttachStream(ctx, stream Stream, mf contract.Manifest, ops *PluginOps, opts ...AttachOption) (*Session, error) // 插件侧·入站
func WithToken(token string) AttachOption          // 帧级握手口令(与 ServeStream 的 token 对等校验)
func ServePluginStream(ctx, stream Stream, mf contract.Manifest, ops *PluginOps) error          // 插件侧·被拨入
func ServeStream(ctx, rh RemoteHost, stream Stream, token string, logf func(...)) error          // 框架侧·入站(校验 token)
func DialStream(ctx, rh RemoteHost, stream Stream, opts DialOptions) (*Adapter, *Session, error) // 框架侧·出站
func NewAdapter(sess *Session, mf contract.Manifest) *Adapter

type DialOptions struct { WantID string }        // 校验远端清单 id 防连错
type AttachOption func(*attachConfig)            // AttachStream 功能选项(加新选项不改签名)
type Adapter struct{ /* Meta/CallFunc/ApplyConfig/ToolSpecs/ToolRaw/Manifest/Session/Close */ }
type PluginOps struct { CallFunc; Tool; Hook func(ctx, hookID, data) any; ApplyConfig; PushEvent func(ctx, topic, e Event) }  // 插件作者侧回调(Hook 返 any 经 HookResultOf;PushEvent 与进程内订阅回调同签名)
type Session struct{ /* Register/CallFunc/Tool/Hook/ApplyConfig/HostCall/Subscribe(ctx, *EventFilter)/Ping/PushEvent(e Event)/Shutdown/Close/OnClosed/Done */ }
```

op 词汇（`HostCall` 能力回程，与 wasm 宿主注入同构）：`call / get_setting /
persist_read / persist_write / persist_append / persist_list / kv_get / kv_put /
kv_delete / kv_keys / exec / on_hook / publish_event / plugins / get_plugin /
get_config / set_config / fs_read / fs_readdir / fs_write / net_request /
net_body_read / net_body_close`，其余 op 经
`Surface.Op` 透传。事件/过滤统一：`publish_event` 载荷是完整 `contract.Event`
JSON，`subscribe` 帧载荷是完整 `contract.EventFilter`。`call_func` / `hook` 帧带
`origin` 字段（调用/触发来源随帧上线，插件侧还原进处理 ctx；加性演进，旧对端
自然忽略）。

## autoload（热更目录装载）

```go
type Options struct { Interval time.Duration; Logf func(...); FS fs.FS; ExtraSuffixes []string }
type Action string  // register | replace | remove | failed
type Event struct { Action; PluginID; Path; Version; Err }

func New(hf loader.HostFace, dirs []string, opts Options) *Watcher
func (w *Watcher) Start(ctx); (w *Watcher) Stop(); (w *Watcher) Poll(ctx) []Event; (w *Watcher) Events() <-chan Event
func (w *Watcher) Unload(ctx)   // 反注册+关闭全部已加载插件(mount 装配失败全清用;正常关停走宿主总闸)
```

增/改/**删**都是两段确认：内容连续两轮一致才装载，文件连续两轮未见才卸载——
目录瞬态读失败、文件系统抖动不误卸已加载插件。**失败重试分层**：内容坏件
(LoadFS 失败/契约拒载)记 errHash——hash 未变不重试(等真变化,重新两段确认
再试);注册瞬态失败(依赖未就位)保留已装载实例,后续轮次**只重试 Register**
(不重编译,依赖到位后注册的正是保留实例)。mount 首装把 Failed 也算进展,
拍至稳定或 maxInitRounds=64 上限。

## mount（一站式装配）

```go
type Sources struct {
    Dirs []string; FS fs.FS                  // wasm 目录(或注入 FS 内的根)
    ExtraSuffixes []string                   // 追加 zip 包后缀(透传 wasm/autoload)
    Watch bool; Interval time.Duration       // 热更
    Remote []RemoteSpec                       // 出站远程 {ID, Addr}(须配 StreamDial)
    StreamDial   func(ctx, addr string) (remote.Stream, error)   // 注入出站传输
    StreamAccept func(ctx) (remote.Stream, error)                // 注入入站传输
    Logf func(...)
}
func Mount(ctx, hf loader.HostFace, src Sources) (*Runtime, []error, error)
type Runtime struct{ /* Events() <-chan autoload.Event; Close() */ }
func CheckDirs(ctx, dirs []string) []error   // 离线校验
```

装配失败时句柄自行**全清**：停 watcher/会话/入站流，并反注册+关闭目录阶段已
进册的插件（`Watcher.Unload`），宿主回到装配前状态；正常关停不做反注册——
注册面归宿主总闸（`Host.Close`）。Close join accept/ServeStream goroutine
(先关流/取消再等,不留迟到注册窗口)。远程会话的入站请求**并发派发**(有界
32/会话):慢 op 不队头阻塞、同会话自调不死锁;请求处理不保序、插件侧
PluginOps 回调可能并发(与进程内插件一致,须线程安全)。

## schema（JSON Schema 子集）

```go
func Validate(schema, value json.RawMessage, root string) []string  // 通用校验(空=通过)
func ValidateConfig(schema, cfg json.RawMessage) []string           // = Validate(root="config")
func Register(name, doc string, proto any)                          // 预设结构目录
func Entries() []Entry; func LookupEntry(name) (Entry, bool)
func BuildConfigSchema(fields []ConfigPair) json.RawMessage
func ValidateMin(name string, payload json.RawMessage) error
func RequiredFields(name string) []string
```

校验支持：`object/properties/required`、`array/items`、`string`（含 `enum`）、
`integer`、`number`、`boolean`、`type` 数组（联合）、`anyOf`。

## undo / exchange

```go
// undo: 统一 effect 栈(Close 时 LIFO 反序清退,错误聚合,幂等)
type Catcher struct{}          // Add(fn func() error); Defer(fn func()); Close() error
func Effect[T any](c *Catcher, acquire func() (T, func())) T // 获取即注册撤销(对应 cordis ctx.effect)
// exchange: 信封翻译表
func Register(name string, proto any)
func NewEvent(point string, payload any, subTypeHint string) contract.Event
func Decode(e contract.Event) (any, error)
func DecodeAs[T any](e contract.Event) (T, error)
```