// 清单词汇(Manifest vocabulary):Meta/Provides/Requires/各声明 spec、SlotSpec 八轴、
// Extensions 自由键值缝(Ext/CloneMeta)与清单帮助函数。
package contract

import "encoding/json"

// ---- 清单词汇 ----

// FuncSpec 是函数/工具的统一声明。同一个可调用体：进"函数面"还是"工具面"
// 由清单的 functions/tools 列表 + tool.provide 能力在注册时区分——spec 共用一种
// 形状,不再有第二个类型。
type FuncSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`  // 入参 JSON Schema
	Output      json.RawMessage `json:"output,omitempty"` // 返回 JSON Schema
	// Extensions 自由扩展键值(非契约轴):同一可调用体的业务元数据/自定义声明,
	// egop 不解释、不校验,只在 JSON 契约里原样透传。上层(如 eha)按 key 读取
	// 自己的语义(值是否 JSON 由上层约定)。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// Ext 从 Extensions 自由键值按 key 取值并 json.Unmarshal 进 T(即 JSON 世界的
// (key string, value any):值形状由声明方与读取方自约定,egop 不解释不校验)。
// 缺键或解码失败返回零值与 false。适用于一切带 Extensions 的结构
// (Meta/FuncSpec/HookPointSpec/EventTopicSpec/ConfigFieldSpec/Dependency/SlotSpec)。
func Ext[T any](ext map[string]json.RawMessage, key string) (T, bool) {
	var zero T
	raw, ok := ext[key]
	if !ok || len(raw) == 0 {
		return zero, false
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return zero, false
	}
	return v, true
}

// CloneMeta 深拷贝 Meta 的全部引用部件(slice/map/RawMessage 字节)。
// 用途:注册边界快照——宿主在 Register/Replace 时克隆一份存册,之后插件侧
// 如何改写自己的 Meta 都不影响宿主目录/校验/控制面视图(注册快照不变量),
// 外发(Plugins/Snapshot)共享同一份冻结拷贝也安全。
func CloneMeta(m Meta) Meta {
	out := m
	out.Provides.Points = cloneStrings(m.Provides.Points)
	out.Provides.Capabilities = cloneStrings(m.Provides.Capabilities)
	out.Provides.Functions = cloneFuncSpecs(m.Provides.Functions)
	out.Provides.Hooks = cloneHookSpecs(m.Provides.Hooks)
	out.Provides.Events = cloneEventSpecs(m.Provides.Events)
	out.Provides.Config = cloneConfigSpecs(m.Provides.Config)
	out.Requires.Listens = cloneStrings(m.Requires.Listens)
	out.Requires.Deps = cloneDeps(m.Requires.Deps)
	out.Requires.Tools = cloneStrings(m.Requires.Tools)
	out.Authors = cloneStrings(m.Authors)
	out.Tags = cloneStrings(m.Tags)
	out.Extensions = cloneRawMap(m.Extensions)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	return append(json.RawMessage(nil), in...)
}

func cloneRawMap(in map[string]json.RawMessage) map[string]json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = cloneRaw(v)
	}
	return out
}

func cloneFuncSpecs(in []FuncSpec) []FuncSpec {
	if in == nil {
		return nil
	}
	out := make([]FuncSpec, len(in))
	for i, f := range in {
		out[i] = f
		out[i].Input = cloneRaw(f.Input)
		out[i].Output = cloneRaw(f.Output)
		out[i].Extensions = cloneRawMap(f.Extensions)
	}
	return out
}

func cloneHookSpecs(in []HookPointSpec) []HookPointSpec {
	if in == nil {
		return nil
	}
	out := make([]HookPointSpec, len(in))
	for i, s := range in {
		out[i] = s
		out[i].Payload = cloneRaw(s.Payload)
		out[i].Result = cloneRaw(s.Result)
		out[i].Extensions = cloneRawMap(s.Extensions)
	}
	return out
}

func cloneEventSpecs(in []EventTopicSpec) []EventTopicSpec {
	if in == nil {
		return nil
	}
	out := make([]EventTopicSpec, len(in))
	for i, s := range in {
		out[i] = s
		out[i].Payload = cloneRaw(s.Payload)
		out[i].Extensions = cloneRawMap(s.Extensions)
	}
	return out
}

func cloneConfigSpecs(in []ConfigFieldSpec) []ConfigFieldSpec {
	if in == nil {
		return nil
	}
	out := make([]ConfigFieldSpec, len(in))
	for i, s := range in {
		out[i] = s
		out[i].Schema = cloneRaw(s.Schema)
		out[i].Default = cloneRaw(s.Default)
		out[i].Extensions = cloneRawMap(s.Extensions)
	}
	return out
}

func cloneDeps(in []Dependency) []Dependency {
	if in == nil {
		return nil
	}
	out := make([]Dependency, len(in))
	for i, d := range in {
		out[i] = d
		out[i].Extensions = cloneRawMap(d.Extensions)
	}
	return out
}

// HookKind 是 hook 点的声明语义:modify=回调可阻断/修改;observe=回调只观察,
// Block 声明无效(宿主在触发收集时丢弃,防"观察者"却阻断流程)。缺省按 modify。
type HookKind string

const (
	KindModify  HookKind = "modify"
	KindObserve HookKind = "observe"
)

// HookPointSpec 对外自有 hook 点。Kind=observe 的点,回调的 Block 声明无效
// (宿主在 TriggerHook 收集结果时按声明丢弃,防"观察者"声明却阻断流程)。
type HookPointSpec struct {
	ID          string          `json:"id"`
	Kind        HookKind        `json:"kind"`
	Description string          `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	// Extensions 自由扩展键值(非契约轴):点的自定义元数据,egop 不解释、不校验,
	// JSON 契约里原样透传;键不应使用 ReservedExtPrefix。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

type EventTopicSpec struct {
	ID          string          `json:"id"`
	Description string          `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	// Extensions 自由扩展键值(非契约轴):主题的自定义元数据,egop 不解释、不校验,
	// JSON 契约里原样透传;键不应使用 ReservedExtPrefix。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

type ConfigFieldSpec struct {
	Key         string          `json:"key"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	// Default 字段默认值(纯声明,供控制面/UI 展示与回填;运行时**真实**默认以插件的
	// ConfigProvider.Config() 返回为准)。
	Default json.RawMessage `json:"default,omitempty"`
	// Readable/Writable 是**跨插件**访问控制(egop/宿主始终可读写)。默认 false:
	// 其它插件不可读/不可写;要开放才设 true。调用方还需声明 config.read 或
	// config.write 能力,两项同时满足才放行。
	Readable bool `json:"readable,omitempty"`
	Writable bool `json:"writable,omitempty"`
	// Secret 标记敏感字段(密钥/口令等):egop 与消费方在日志、快照、展示时应脱敏。
	// 宿主下发配置的观察事件按它**声明优先**脱敏(顶层命中即遮,键名启发兜底)。
	Secret bool `json:"secret,omitempty"`
	// Extensions 自由扩展键值(非契约轴):字段的自定义元数据,egop 不解释、不校验,
	// JSON 契约里原样透传;键不应使用 ReservedExtPrefix。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
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

// Dependency 一条依赖。Plugin 与 Slot 必须恰有其一(双空在注册口拒载;
// 双取时 Slot 优先生效,与装载排序/卸载判定同款)。未知 Kind 前向兼容地忽略
// (不参与任何校验),待未来语义定义。
type Dependency struct {
	Plugin     string         `json:"plugin,omitempty"`
	Slot       string         `json:"slot,omitempty"`
	Kind       DependencyKind `json:"kind"`
	MinVersion string         `json:"min_version,omitempty"`
	// Extensions 自由扩展键值(非契约轴):依赖的自定义元数据,egop 不解释、不校验,
	// JSON 契约里原样透传;键不应使用 ReservedExtPrefix。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
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

	// Extensions 自由扩展键值(非契约轴,完全由开发者自行约定;egop 不解释、不校验,
	// 只在 JSON 契约里原样透传——供目录/控制面/其它插件按 key 读取自定义能力声明、
	// 业务元数据等)。值是否 JSON 由开发者约定,egop 不做格式约束。
	// 键不应使用 ReservedExtPrefix("egop." 保留给 egop 自身特性,如 "egop.pool")。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
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
	Builtin bool     `json:"builtin,omitempty"`
	// Extensions 自由扩展键值(非契约轴):槽位的自定义元数据(展示分组/装配提示等),
	// 由槽位定义方(装配层)与消费方自约定;egop 不解释、不校验。与 Meta.Extensions 同构。
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

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
