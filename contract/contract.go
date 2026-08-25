// Package contract 是插件机制核心的**契约词汇**（内容无关、无任何业务域引用）：
// 清单/槽位契约/依赖词汇、交换信封、动态前缀——外部业务域以本包为扩展点注册
// 自己的形状与槽位。
package contract

import (
	"context"
	"encoding/json"
	"io"
	"strings"
)

// DynamicPrefix 动态点位/主题 id 的固定前缀。
const DynamicPrefix = "dyn."

// 预置能力词常量族（只内置宿主能力面门控所需的固定词；业务词由消费方自定义）。
const (
	CapCallPlugins   = "plugin.call"     // 经宿主调用别的插件函数
	CapPluginMeta    = "plugin.meta"     // 读插件目录/其它插件元数据
	CapEmitsEvents   = "event.emit"      // 发布事件
	CapListensEvents = "event.listen"    // 订阅事件广播
	CapPersist       = "storage.persist" // 插件专属文件读写
	CapKV            = "storage.kv"      // 插件专属 KV
	CapExec          = "exec.cmd"        // 执行命令
	CapNet           = "net.access"      // 出站网络(HTTP/HTTPS/SSE/WebSocket/WebTransport 等,经 Net 注入)
	CapTools         = "tool.provide"    // 向工具面提供工具
	CapConfigRead    = "config.read"     // 读其它插件的声明配置字段
	CapConfigWrite   = "config.write"    // 写其它插件的声明配置字段
)

// EnumHinter 枚举提示契约（目录生成器按它产出 enum）。
type EnumHinter interface {
	EnumValues() []string
}

// ---- 交换信封 ----

// EnvelopeVersion 信封定级常数。
const EnvelopeVersion = 1

// OriginKind 是消息来源的类别。
type OriginKind string

const (
	OriginEvent OriginKind = "event" // 事件发布
	OriginHook  OriginKind = "hook"  // hook 触发
	OriginCall  OriginKind = "call"  // 跨插件函数调用
	OriginHost  OriginKind = "host"  // 宿主/框架自身
)

// Origin 描述一条消息/事件/回调的**来源**(溯源上下文)。框架在派发时固定填好,
// 插件无需也不应手动构造。
type Origin struct {
	ID      string     `json:"id,omitempty"`      // 来源插件 id(空 = 宿主/框架)
	Version string     `json:"version,omitempty"` // 来源插件版本
	Kind    OriginKind `json:"kind,omitempty"`    // 来源类别
	Point   string     `json:"point,omitempty"`   // 触发点位(主题/钩子点/函数名)
	At      int64      `json:"at,omitempty"`      // 来源产生时间戳(ms)
}

// originKey 是 ctx 里携带"调用来源"的内部键(跨插件函数调用时由宿主注入)。
type originKey struct{}

// WithOrigin 把调用来源写入 ctx(框架内部用,插件一般无需调用)。插件发起跨插件
// 调用时宿主自动注入调用者 Origin;被调函数经 OriginFrom 读取。
func WithOrigin(ctx context.Context, o *Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

// OriginFrom 读 ctx 里的调用来源;无则返回 nil(表示由宿主/应用直接发起,非插件调用)。
func OriginFrom(ctx context.Context) *Origin {
	o, _ := ctx.Value(originKey{}).(*Origin)
	return o
}

// Event 统一交换信封（元数据 + 原始 JSON 载荷）。
// 投递/扇出时同一个 Event 值会被多个订阅者共享:Labels 是 map、Source 是指针,
// 均为引用——**订阅者须按只读使用,不得改写**(否则污染其它订阅者与发布者)。
type Event struct {
	Type    string          `json:"type"`
	SubType string          `json:"sub_type,omitempty"`
	Version int             `json:"version,omitempty"`
	Source  *Origin         `json:"source,omitempty"` // 来源(框架填;含时间戳 Source.At)
	Payload json.RawMessage `json:"payload"`
	// Labels 发布者附带的事件标签(自由键值,供订阅过滤/审计);框架不解释不写入。
	Labels map[string]string `json:"labels,omitempty"`
}

// EventFilter 描述订阅要命中的事件条件。**nil 或零值 = 命中一切事件**;设置的字段
// 一起做 AND 匹配(可只设一个,也可设多个)。匹配对象是事件的**上下文字段**——
// 事件自有字段(Type/SubType/Labels)与来源 Origin 的 id/version/kind(Point 即 Type,
// At 为时间戳,均不适合作等值键,故不参与过滤)。Payload 是不透明 JSON,不做深层匹配
// (保持内容无关)。
type EventFilter struct {
	Type          string            `json:"type,omitempty"`           // 主题;含 '*' 按通配匹配('*' = 任意字符序列,含空)
	SubType       string            `json:"sub_type,omitempty"`       // 子类型精确
	SourceID      string            `json:"source_id,omitempty"`      // 来源插件 id(Origin.ID)
	SourceVersion string            `json:"source_version,omitempty"` // 来源插件版本(Origin.Version)
	SourceKind    OriginKind        `json:"source_kind,omitempty"`    // 来源类别(Origin.Kind)
	Labels        map[string]string `json:"labels,omitempty"`         // 要求事件带这些键值(子集相等)
}

// Match 判定事件是否命中本过滤条件(空字段不约束)。
func (f EventFilter) Match(e Event) bool {
	if f.Type != "" && !globMatch(f.Type, e.Type) {
		return false
	}
	if f.SubType != "" && e.SubType != f.SubType {
		return false
	}
	if f.SourceID != "" && (e.Source == nil || e.Source.ID != f.SourceID) {
		return false
	}
	if f.SourceVersion != "" && (e.Source == nil || e.Source.Version != f.SourceVersion) {
		return false
	}
	if f.SourceKind != "" && (e.Source == nil || e.Source.Kind != f.SourceKind) {
		return false
	}
	for k, v := range f.Labels {
		if got, ok := e.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// globMatch 简易 '*' 通配:'*' 匹配任意字符序列(含空),无 '*' 时退化为精确相等。
func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// ResultEnvelope 跨世界调用的统一结果信封。
// type/at/meta 为可选上下文：type 结果内容类型,at 时间戳(ms),meta 开放元数据
// (correlation id、来源插件等,由生产/消费方约定)。
type ResultEnvelope struct {
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	ResultB64 string          `json:"result_b64,omitempty"`
	Error     string          `json:"error,omitempty"`
	Type      string          `json:"type,omitempty"`
	At        int64           `json:"at,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
}

// ---- 清单词汇 ----

// FuncSpec 是函数/工具的统一声明。同一个可调用体：进"函数面"还是"工具面"
// 由清单的 functions/tools 列表 + tool.provide 能力在注册时区分——spec 共用一种
// 形状,不再有第二个类型。
type FuncSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`  // 入参 JSON Schema
	Output      json.RawMessage `json:"output,omitempty"` // 返回 JSON Schema
}

type HookKind string

const (
	KindModify  HookKind = "modify"
	KindObserve HookKind = "observe"
)

type HookPointSpec struct {
	ID      string          `json:"id"`
	Kind    HookKind        `json:"kind"`
	Desc    string          `json:"desc,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type EventTopicSpec struct {
	ID          string          `json:"id"`
	Description string          `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type ConfigFieldSpec struct {
	Key         string          `json:"key"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	// Readable/Writable 是**跨插件**访问控制(egop/宿主始终可读写)。默认 false:
	// 其它插件不可读/不可写;要开放才设 true。调用方还需声明 config.read 或
	// config.write 能力,两项同时满足才放行。
	Readable bool `json:"readable,omitempty"`
	Writable bool `json:"writable,omitempty"`
	// Secret 标记敏感字段(密钥/口令等):egop 与消费方在日志、快照、展示时应脱敏。
	Secret bool `json:"secret,omitempty"`
}

type DependencyKind string

const (
	// DepInit 硬依赖:注册时须已就位,卸载时 fail-closed 或级联(连坐)。
	DepInit DependencyKind = "init"
	// DepCall 跨插件调用关系:声明「本插件会调用对方函数」,配合 plugin.call 能力。
	DepCall DependencyKind = "call"
	// DepSoft 软依赖:不参与装载排序、不拦卸载;依赖方应订阅 plugin.removed 等
	// 生命周期事件自行降级(响应式 coeffect 的声明面)。
	DepSoft DependencyKind = "soft"
)

type Dependency struct {
	Plugin     string         `json:"plugin,omitempty"`
	Slot       string         `json:"slot,omitempty"`
	Kind       DependencyKind `json:"kind"`
	MinVersion string         `json:"min_version,omitempty"`
}

// Provides 插件自述的对外供给面（我声明自己"给"什么）。
type Provides struct {
	Points       []string          `json:"points,omitempty"`       // 保证发射的框架点位
	Hooks        []HookPointSpec   `json:"hooks,omitempty"`        // 对外自有 hook 点
	Events       []EventTopicSpec  `json:"events,omitempty"`       // 对外发布的事件主题
	Functions    []FuncSpec        `json:"functions,omitempty"`    // 对外可调函数
	Capabilities []string          `json:"capabilities,omitempty"` // 能力词
	Config       []ConfigFieldSpec `json:"config,omitempty"`       // 可下发配置字段
}

// Requires 插件对外依赖面（我声明自己"要"什么）。
type Requires struct {
	Listens []string     `json:"listens,omitempty"` // 要订阅的框架点位
	Deps    []Dependency `json:"deps,omitempty"`    // 依赖（点名/点槽位 + kind/版本）
	Tools   []string     `json:"tools,omitempty"`   // 依赖的工具面
}

// Meta 是插件元数据：自述供给面(Provides) + 对外依赖面(Requires) + 槽位(Slot)。
type Meta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`

	// 描述性元数据(非契约轴,供目录/控制面展示与检索;均可选):
	Homepage string   `json:"homepage,omitempty"` // 项目/文档主页
	License  string   `json:"license,omitempty"`  // SPDX 许可证标识(如 MIT / Apache-2.0)
	Authors  []string `json:"authors,omitempty"`  // 作者
	Tags     []string `json:"tags,omitempty"`     // 关键词/分类

	Provides Provides `json:"provides,omitempty"`
	Requires Requires `json:"requires,omitempty"`
	Slot     string   `json:"slot,omitempty"`

	DependsOn []string `json:"depends_on,omitempty"` // 遗留纯声明
}

// Manifest 线上清单：Meta 平铺 + 工具声明（与函数共用 FuncSpec 型）。
type Manifest struct {
	Meta
	Tools []FuncSpec `json:"tools,omitempty"`
}

// ---- 槽位契约（实现了"点/钩/事件/函数/能力/配置/监听/工具"八轴 + 前置槽位）----
//
// 字段名与 Meta（Provides/Requires）同轴字段保持一致：SlotSpec 只列名字、Meta
// 再配 Schema。Needs 是槽位专属的前置槽位清单；Requires.Deps 是带 kind/版本的
// 富依赖，形状不同，不强求同名。

type SlotSpec struct {
	ID  string `json:"id"`
	Doc string `json:"doc"`

	Provides     []string `json:"provides,omitempty"`
	Hooks        []string `json:"hooks,omitempty"`
	Events       []string `json:"events,omitempty"`
	Functions    []string `json:"functions,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Config       []string `json:"config,omitempty"`
	Listens      []string `json:"listens,omitempty"`
	// NeedsTools 槽位必备的工具面（实现声称时,须在 Meta.Requires.Tools 覆盖本轴）。
	NeedsTools []string `json:"needs_tools,omitempty"`

	Needs   []string `json:"needs,omitempty"`
	Builtin bool     `json:"builtin"`
}

// ---- 插件接口族 ----

type Plugin interface {
	Meta() Meta
}

type Configurable interface {
	ApplyConfig(cfg json.RawMessage) error
}

type ConfigProvider interface {
	Config() json.RawMessage
}

type FunctionProvider interface {
	CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error)
}

type ToolFunc[C any] func(ctx context.Context, tctx *C, input json.RawMessage) (json.RawMessage, error)

type ToolProvider[C any] interface {
	ToolSpecs() []FuncSpec
	Tool(name string) (ToolFunc[C], bool)
}

// HookFunc 是 hook 回调签名。回调可返回两种形态,框架统一归一:
//   - contract.HookResult:带上下文的完整形态(Block/Reason/Data 由回调写,
//     Who/At/Seq 由框架填);
//   - 直接返回数据(nil / json.RawMessage / []byte / string / 数值 / 结构体 /
//     map / 切片等):框架经 HookResultOf 包成 HookResult(Block=false,Data=该值
//     的 JSON 编码)。
type HookFunc func(ctx context.Context, hookID string, data json.RawMessage) any

// HookResult 是 hook 回调的归一结果。
// Block/Reason/Data 由**回调**写入(或由 HookResultOf 从原始数据归一);Who/At/Seq
// 由**框架**在触发时填充——这样结果既有"阻断 + 描述理由 + 产出数据",又带
// "谁产生、何时、第几个"的执行上下文。
type HookResult struct {
	Block  bool            `json:"block,omitempty"`  // 回调写:是否阻断后续
	Reason string          `json:"reason,omitempty"` // 回调写:描述性理由
	Data   json.RawMessage `json:"data,omitempty"`   // 回调写:产出数据(自带上下文)
	Origin *Origin         `json:"origin,omitempty"` // 框架填:来源(hook 触发)
	Seq    int             `json:"seq,omitempty"`    // 框架填:回调顺序(1 起)
}

// HookResultOf 把 hook 回调的返回值归一成 HookResult(仅回调写入的部分;Origin/Seq
// 由框架在触发时回填)。规则:
//   - nil → HookResult{}
//   - HookResult / *HookResult → 原样
//   - json.RawMessage / []byte → 视作已是 JSON 字节,放进 Data
//   - 其它(string/数值/bool/struct/map/切片) → json.Marshal 后放进 Data
//   - 序列化失败 → Reason 记错误(仍返回非阻断结果)
func HookResultOf(v any) HookResult {
	switch t := v.(type) {
	case nil:
		return HookResult{}
	case HookResult:
		return t
	case *HookResult:
		if t == nil {
			return HookResult{}
		}
		return *t
	case json.RawMessage:
		return HookResult{Data: t}
	case []byte:
		return HookResult{Data: json.RawMessage(t)}
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return HookResult{Reason: "hook: marshal data: " + err.Error()}
		}
		return HookResult{Data: b}
	}
}

// FileStore/KeyValue 插件专属存储面。
type FileStore interface {
	Read(name string) ([]byte, error)
	Write(name string, data []byte) error
	List() ([]string, error)
}

type KeyValue interface {
	Get(key string) ([]byte, bool)
	Put(key string, v []byte)
	Delete(key string)
	Keys() []string
}

// Storage 是插件持久化的**注入后端**(与 io/fs.FS 读侧、网络 Stream 同理):装配层
// 提供,宿主把**原始 pluginID** 转发给实现——命名空间/目录布局/hash 等存储策略由实现
// 自行决定(egop 不越权)。File/KV 各返回该插件专属的隔离存储;返回 nil 表示不提供
// 该项(未注入或后端无该项时,Surface.Persist/KeyValue 即不可用)。
type Storage interface {
	File(pluginID string) FileStore
	KV(pluginID string) KeyValue
}

// Stream 是双向二进制消息流的最小面:一条消息 = 一次 Send/Recv。
// 同一形状覆盖 WebSocket 消息、WebRTC DataChannel、WebTransport 双向流、
// MQTT-over-WS(插件在消息体里自述 MQTT 语义)等所有"消息型"传输——
// egop 只按字节消息收发,不解析任何传输协议。
type Stream interface {
	Send([]byte) error
	Recv() ([]byte, error)
	Context() context.Context
}

// Request 是一次出站请求。HTTP/HTTPS 之外,SSE(流式长响应)、gRPC-Web、
// JSON-RPC、GraphQL、Connect、普通 REST 均统一于此——它们都是"请求 + 响应体"。
// 客户端流式(如 gRPC-Web client-streaming)靠 Body 持续读实现。
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    io.Reader         `json:"-"` // 可流;nil = 无请求体
}

// Response 是出站响应(状态 + 头 + 可流 body)。SSE 场景 Body 是持续事件流,
// 可一直读;Trailers 承载 gRPC(-Web)/HTTP2 的尾部元数据(连接闭合后才可用)。
type Response struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	Trailers map[string]string `json:"trailers,omitempty"` // gRPC(-Web)/HTTP2 trailer
	Body     io.Reader         `json:"-"`                  // 可流;读完即结束
}

// Net 是插件**出站网络**的注入后端:egop 只定义最小面与数据结构,不实现任何传输。
// 桌面装配层用 net/http + websocket 实现,浏览器 wasm 用 fetch/WebSocket/WebTransport
// 实现——Request 覆盖单向请求族(HTTP/HTTPS/SSE/gRPC-Web/JSON-RPC/GraphQL/REST),
// DialStream 覆盖双向消息流族(WebSocket/WebRTC DataChannel/WebTransport/MQTT-over-WS)。
// egop 自身零网络依赖,具体传输能力由该后端与目标平台共同决定。
type Net interface {
	// Request 发起一次 HTTP(S) 请求;SSE 经 resp.Body 持续读事件流,
	// gRPC(-Web) unary 经 resp.Trailers 读尾部元数据。
	Request(ctx context.Context, req Request) (*Response, error)
	// DialStream 建立一条双向消息流:URL scheme 决定传输——ws/wss 即 WebSocket、
	// https(HTTP/3)即 WebTransport,或装配层自定义的其它 scheme。
	// egop 只按字节消息收发,不解析 WS/WebTransport 协议。
	DialStream(ctx context.Context, url string, headers map[string]string) (Stream, error)
}

// Surface 是宿主向插件暴露的**能力面**：基础能力有固定方法，扩展能力一律经
// Op(name, input) 自由调用（宿主注入分发表）。
type Surface interface {
	Plugins() []Meta
	GetPlugin(id string) (Meta, bool)
	Call(ctx context.Context, pluginID, fname string, input json.RawMessage) (json.RawMessage, error)
	GetSetting(key string) (json.RawMessage, bool)
	PublishEvent(ctx context.Context, topic string, payload json.RawMessage)
	// Publish 发布一个完整事件:调用方给 Type(=主题)/SubType/Labels/Payload,
	// 框架回填 Version 与 Source(来源身份/类别/点位/时间戳)。
	Publish(ctx context.Context, e Event)
	SubscribeEvent(topic string, fn func(ctx context.Context, topic string, e Event)) func()
	// SubscribeEventFilter 按过滤条件订阅(可只设一个或多个上下文字段);nil 或零值
	// filter 命中一切事件。回调收到的 e 为多个订阅者共享的只读事件,不得改写。
	SubscribeEventFilter(f *EventFilter, fn func(ctx context.Context, topic string, e Event)) func()
	OnHook(hookID string, fn HookFunc) func()
	Persist() (FileStore, bool)
	KV() (KeyValue, bool)
	Net() (Net, bool) // 出站网络(需 net.access 能力且装配层注入 Net)
	Exec(ctx context.Context, cmd string) (string, error)
	Op(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error)
	// GetConfig 读其它插件声明的一个配置字段。需 caller 声明 config.read 能力,且
	// 该字段 Readable=true;否则返回 (nil,false)。
	GetConfig(pluginID, key string) (json.RawMessage, bool)
	// SetConfig 写其它插件声明的一个配置字段。需 caller 声明 config.write 能力,且
	// 该字段 Writable=true;否则返回 error。
	SetConfig(pluginID, key string, value json.RawMessage) error
}

// SurfaceAware 插件声明想要接收宿主能力面（注册时注入）。
type SurfaceAware interface {
	SetSurface(s Surface)
}

// Disposer 可选：插件持有需要显式清退的资源（wasm 实例/网络会话等）。
// 注意：仅宿主整体 `Host.Close` 会调用它；`Host.Remove`/`Replace` 只清 effect 栈
// 与目录、**不**调用 Disposer——单件卸载/热替换的调用方须自行 Close 插件句柄
// (autoload 正是这么做的:Remove/Replace 后立即 plugin.Close)。
type Disposer interface {
	Close(ctx context.Context) error
}

// ---- 能力词与配置键占位框架 ----

// EventConfigUpdated 是宿主下发配置成功的观察事件主题
// (payload = {"plugin":id,"config":cfg};经 Events 总线广播)。
const EventConfigUpdated = "plugin.config.updated"

// EventPluginRegistered / EventPluginRemoved / EventPluginReplaced 是宿主插件
// 生命周期观察事件主题(payload = {"plugin":id,"version":v})。软依赖(DepSoft)方
// 订阅这些主题做响应式降级(对应 cordis 的"响应式 coeffect":上下文变化即通知)。
const (
	EventPluginRegistered = "plugin.registered"
	EventPluginRemoved    = "plugin.removed"
	EventPluginReplaced   = "plugin.replaced"
)

// HasCapability 声明判定。
func HasCapability(m Meta, cap string) bool {
	for _, c := range m.Provides.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// PointID/EventID 命名空间化。
func PointID(pluginID, pointID string) string { return DynamicPrefix + pluginID + "." + pointID }
func EventID(pluginID, short string) string   { return DynamicPrefix + pluginID + "." + short }
