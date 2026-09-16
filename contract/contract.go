// Package contract 是插件机制核心的**契约词汇**（内容无关、无任何业务域引用）：
// 清单/槽位契约/依赖词汇、交换信封、动态前缀——外部业务域以本包为扩展点注册
// 自己的形状与槽位。
package contract

import (
	"context"
	"encoding/json"
	"strings"
)

// DynamicPrefix 动态点位/主题 id 的固定前缀。
const DynamicPrefix = "dyn."

// ReservedExtPrefix 是 Extensions 自由键值的保留前缀:egop 自身特性(如实例池
// 大小 "egop.pool")一律经该前缀键约定;开发者自定义键**不应**使用它——未来
// egop 版本可能在该前缀下引入新键,撞名即语义漂移。反向同理:egop 绝不解释
// 任何非保留前缀键。
const ReservedExtPrefix = "egop."

// 预置能力词常量族（只内置宿主能力面门控所需的固定词；业务词由消费方自定义）。
const (
	CapCallPlugins   = "plugin.call"     // 经宿主调用别的插件函数
	CapPluginMeta    = "plugin.meta"     // 读插件目录/其它插件元数据
	CapEmitsEvents   = "event.emit"      // 发布事件
	CapListensEvents = "event.listen"    // 订阅事件广播
	CapPersist       = "storage.persist" // 插件专属文件读写(隔离目录)
	CapKV            = "storage.kv"      // 插件专属 KV
	CapExec          = "exec.cmd"        // 执行命令
	CapNet           = "net.access"      // 出站网络(HTTP/HTTPS/SSE/WebSocket/WebTransport 等,经 Net 注入)
	CapFSRead        = "fs.read"         // 全局文件系统读取(经 FS 注入;区别于 storage.persist 的插件专属隔离目录)
	CapFSWrite       = "fs.write"        // 全局文件系统写入(同上;读写分别门控)
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
	EventPluginFailed     = "plugin.failed" // 懒插件补载硬失败(依赖到位后被拒),payload 含 error
)

// frameworkTopics 框架保留主题全集(宿主专署广播:配置/生命周期观察事件)。
var frameworkTopics = map[string]bool{
	EventConfigUpdated:    true,
	EventPluginRegistered: true,
	EventPluginRemoved:    true,
	EventPluginReplaced:   true,
	EventPluginFailed:     true,
}

// IsFrameworkTopic 判定主题是否框架保留:插件经 Surface 不可发布这些主题
// (宿主在能力门控视图单点拒发并留痕)——防止插件伪造 plugin.removed 等生命周期
// 事件欺骗软依赖方/控制面。框架侧广播(宿主)不受限。
func IsFrameworkTopic(topic string) bool { return frameworkTopics[topic] }
