// 插件接口族与注入后端面:Plugin 可选能力接口、HookFunc/HookResult、
// FileStore/KeyValue/Storage、FS/DirEntry、Stream/Request/Response/Net、Surface。
package contract

import (
	"context"
	"encoding/json"
	"io"
)

// ---- 插件接口族 ----

type Plugin interface {
	Meta() Meta
}

type Configurable interface {
	ApplyConfig(cfg json.RawMessage) error
}

// ConfigProvider 可选:插件自述**当前生效配置**(权威读回,与 Configurable.ApplyConfig
// 成对的"读"面)。宿主读配置优先走它,未实现则回退 host.applied 缓存——它覆盖了
// 默认值补齐/归一化/脱敏后的真值(web 配置界面拿它显示真实状态)。
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
	// Append 末尾追加(不重写既有内容;append-only 日志/事实账的机制真源——
	// Write 是整文件覆盖语义,拿它追加=每次截断只留末行)。
	Append(name string, data []byte) error
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

// FS 是插件**全局文件系统**面的注入后端(与 Storage/Net 同一注入哲学:egop 只定义
// 最小面、不实现任何平台 IO)。与 storage.persist 的插件专属隔离目录不同,这是装配层
// 授予插件的一个显式受控主机文件视图——可见范围/沙箱/路径白名单策略完全由实现决定
// (装配层可给 io/fs.Sub 的子树视图、只读镜像等)。读写分别门控(fs.read / fs.write):
// 能力判定在 Surface 视图层做,未声明者拿不到面/写被拒;实现只需关心 IO 语义。
type FS interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
	// ReadDir 列举目录一层条目(不递归)。目录类插件(技能/文件浏览)的最小
	// 发现面;JSON 友好形状,跨 wasm ABI/远程帧原样传输。
	ReadDir(name string) ([]DirEntry, error)
}

// DirEntry 目录列举的一项(ReadDir 结果最小形状;名字+是否目录,细节留实现方)。
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	// Size/ModTime 是增量元数据(2026-09-06,fsp zip 化前置:列表/检索工具
	// 需尺寸与时间;旧 guest 忽略新字段,ABI JSON 增量安全)。目录 Size=0。
	Size    int64 `json:"size,omitempty"`
	ModTime int64 `json:"mod_time,omitempty"` // unix 毫秒;0=未知
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
	// FS 返回全局文件系统面(需 fs.read / fs.write 至少其一,且装配层注入 FS)。
	// 读写按声明分别门控:未声明 fs.read 时 ReadFile 报错,未声明 fs.write 时
	// WriteFile 报错(先说后做,与 Net 协议门同款单点强制)。
	FS() (FS, bool)
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
