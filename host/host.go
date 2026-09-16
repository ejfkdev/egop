// Package host 是插件宿主的**内容无关核心**（泛化 C=工具上下文类型）：
// 注册/卸载/热替换/函数目录/工具包装/配置 Schema 校验/槽位八轴+Needs 校验——
// 业务能力一律经装配注入(Op 扩展),文件/网络/日志等底层能力也由外部注入,词汇见契约包。
package host

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/undo"
)

// Points 点位总线消费口（宿主借它 EnsurePoint 落点）。
type Points interface {
	EnsurePoint(point string)
}

// Events 事件广播消费口（过滤式订阅/发布）。
type Events interface {
	// Subscribe 按过滤条件订阅(nil 或零值 = 命中一切);回调收到命中事件的完整 Event。
	Subscribe(f *contract.EventFilter, fn func(context.Context, contract.Event)) func()
	// Dispatch 广播一条事件;Source/Version 由调用方(宿主 Surface)先填好。
	Dispatch(ctx context.Context, e contract.Event)
	EnsureTopic(topic string)
}

// Options 宿主装配选项（零值可用）。
type Options[C any] struct {
	Points   Points
	Events   Events
	Hooks    Hooks
	Settings Source
	// Storage 持久化注入后端(必填;nil → Persist/KV 不可用)。
	Storage contract.Storage
	Net     contract.Net // 出站网络注入后端(必填;nil → Net 不可用)
	// FS 全局文件系统注入后端(nil → Surface.FS 不可用)。可见范围/沙箱策略由
	// 实现决定;能力门控(fs.read/fs.write 分向)由宿主 fsGuard 单点强制。
	FS contract.FS
	// NetSchemes 补充允许的**网络协议 scheme**(小写,如 "webtransport"、自定义)。
	// 内置 http/https/ws/wss 始终允许;file:// 等本地/特殊 scheme 一律拒绝。
	NetSchemes []string
	ExecFn     func(ctx context.Context, cmd string) (string, error)
	Ops        map[string]Op     // 扩展能力:能力词 → 处理器(声明才可调)
	OpAliases  map[string]string // wire 短名 → 能力词(装配层注入的守卫别名)
	ToolNames  func() []string   // 框架已就位的工具面(八轴校验用)
	SlotLookup func(id string) (contract.SlotSpec, bool)
	// DisableFuncValidation 关闭宿主对函数入参/返回的 schema 校验(默认 false=
	// 开启)。仅对声明了 FuncSpec.Input/Output 的函数生效。
	DisableFuncValidation bool
	// Logf 生命周期日志口(注册/替换/卸载/配置等;nil = 静默)。
	Logf func(format string, args ...any)
}

// OpAlias 查 wire 短名的守卫能力词(未声明别名时短名即能力词)。
func (o Options[C]) opCap(name string) string {
	if o.OpAliases != nil {
		if cap, ok := o.OpAliases[name]; ok {
			return cap
		}
	}
	return name
}

// Op 是扩展能力处理器（装配层经 Options.Ops 注入;插件声明守卫词后经 Surface.Op 可调）。
type Op func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

// Source 别名 contract.Source（装配口）。
type Source = contract.Source

// Host 是泛化插件宿主。
type Host[C any] struct {
	mu      sync.Mutex
	plugins map[string]contract.Plugin
	meta    map[string]contract.Meta
	fns     map[string]fnEntry
	opts    Options[C]
	applied map[string]json.RawMessage
	seq     map[string]uint64 // 注册序(Close 逆序清退用)
	nextSeq uint64
	effects map[string]*undo.Catcher // 每个插件注册的 effect 撤销栈(Remove/Replace 自动回滚)

	// cfgMu 串行化配置写链路(SetConfig/SetConfigField 的读改写全程):并发下发
	// 不交错、单字段合并不丢更新。配置写是控制面低频操作,串行代价可忽略;
	// 与 mu 分离,ApplyConfig(插件代码)执行期间不占用宿主目录锁。
	cfgMu sync.Mutex

	pending        []string                   // 懒加载待补载 id(保持插入序)
	pendingPlugins map[string]contract.Plugin // id → 待补载插件
	netSchemes     map[string]bool            // 出站网络允许的协议 scheme(小写)

	// hookDecls 是声明面自有 hook 点的登记(hook id → 声明者+Kind)。
	// Kind=observe 的点,TriggerHook 收集结果时按声明丢弃回调的 Block
	// (声明优先:观察者不得阻断)。注册/替换/删除后重建(注册序=首声明者序)。
	hookDecls map[string]hookDecl
}

// hookDecl 记一个 hook 点的声明者与语义。
type hookDecl struct {
	owner string
	kind  contract.HookKind
}

type fnEntry struct {
	pluginID string
	spec     contract.FuncSpec
	provider contract.FunctionProvider
}

// New 构造宿主。开箱默认(opts 零值也可用):Events=内存总线 MemEvents、
// Settings=MapSettings、Points=MemPoints——全部可装配注入替换。文件、网络、日志等
// 底层能力一律由装配层注入(Storage/ExecFn/Logf),egop 自身不内置。
func New[C any](opts Options[C]) *Host[C] {
	if opts.Events == nil {
		opts.Events = NewMemEvents()
	}
	if opts.Hooks == nil {
		opts.Hooks = NewMemHooks()
	}
	if opts.Settings == nil {
		opts.Settings = NewMapSettings()
	}
	if opts.Points == nil {
		opts.Points = NewMemPoints()
	}
	opts.Events.EnsureTopic(contract.EventConfigUpdated)
	opts.Events.EnsureTopic(contract.EventPluginRegistered)
	opts.Events.EnsureTopic(contract.EventPluginRemoved)
	opts.Events.EnsureTopic(contract.EventPluginReplaced)
	opts.Events.EnsureTopic(contract.EventPluginFailed)
	return &Host[C]{
		plugins:        map[string]contract.Plugin{},
		meta:           map[string]contract.Meta{},
		fns:            map[string]fnEntry{},
		opts:           opts,
		applied:        map[string]json.RawMessage{},
		seq:            map[string]uint64{},
		effects:        map[string]*undo.Catcher{},
		pendingPlugins: map[string]contract.Plugin{},
		netSchemes:     buildNetSchemes(opts.NetSchemes),
		hookDecls:      map[string]hookDecl{},
	}
}

func (h *Host[C]) logf(format string, args ...any) {
	if h.opts.Logf != nil {
		h.opts.Logf(format, args...)
	}
}

// emitLifecycle 广播插件生命周期观察事件(plugin.registered/removed/replaced)。
// 软依赖(DepSoft)方订阅这些主题做响应式降级;Source 为宿主(Kind=host)。
func (h *Host[C]) emitLifecycle(topic, id, version string) {
	if h.opts.Events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"plugin": id, "version": version})
	h.opts.Events.Dispatch(context.Background(), contract.Event{
		Type:    topic,
		Version: contract.EnvelopeVersion,
		Source:  &contract.Origin{Kind: contract.OriginHost, Point: topic, At: time.Now().UnixMilli()},
		Payload: payload,
	})
}

// injectSurface 向插件注入能力面视图;SetSurface 属插件代码,panic 归一为日志
// (注册/替换已入册,面注入 best-effort,不 crash 宿主)。
func (h *Host[C]) injectSurface(sa contract.SurfaceAware, id string, s contract.Surface) {
	defer func() {
		if p := recover(); p != nil {
			h.logf("host: plugin %s SetSurface panicked: %v", id, p)
		}
	}()
	sa.SetSurface(s)
}

// Close 统一关停宿主:按**注册逆序**移除全部插件,并对实现 contract.Disposer
// 的插件(wasm 实例/远程会话等)执行清退;清理错误聚合返回(尽力而为,不中断)。
func (h *Host[C]) Close(ctx context.Context) error {
	h.mu.Lock()
	type row struct {
		id  string
		seq uint64
		p   contract.Plugin
	}
	rows := make([]row, 0, len(h.plugins))
	for id, p := range h.plugins {
		rows = append(rows, row{id: id, seq: h.seq[id], p: p})
	}
	h.mu.Unlock()
	// 注册逆序:后注册者(通常是依赖方)先清退
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq > rows[j].seq })
	var errs []error
	for _, r := range rows {
		if d, ok := r.p.(contract.Disposer); ok {
			if err := closeDisposer(d, ctx, r.id); err != nil {
				errs = append(errs, err)
			}
		}
		if _, err := h.Remove(r.id, true); err != nil {
			errs = append(errs, err)
		}
	}
	// 尚未补载的懒插件(依赖始终未到位)也一并清退:Disposer 资源(wasm 实例等)不泄漏。
	h.mu.Lock()
	type pendingRow struct {
		id string
		p  contract.Plugin
	}
	pending := make([]pendingRow, 0, len(h.pendingPlugins))
	for id, p := range h.pendingPlugins {
		pending = append(pending, pendingRow{id: id, p: p})
	}
	h.pending = nil
	h.pendingPlugins = map[string]contract.Plugin{}
	h.mu.Unlock()
	for _, pr := range pending {
		if d, ok := pr.p.(contract.Disposer); ok {
			if err := closeDisposer(d, ctx, pr.id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Plugins 注册序清单;HasPlugin 判定;AppliedConfig 效配置。
func (h *Host[C]) Plugins() []contract.Meta {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]contract.Meta, 0, len(h.meta))
	for _, m := range h.meta {
		out = append(out, m)
	}
	// 按注册序返回(meta 是 map,直接遍历顺序不定;seq 即注册序)。
	sort.Slice(out, func(i, j int) bool { return h.seq[out[i].ID] < h.seq[out[j].ID] })
	return out
}

func (h *Host[C]) HasPlugin(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.plugins[id]
	return ok
}

// Dependents 返回以 DepInit(点名或槽位)依赖该插件的在册插件 id 列表
// (元数据反查:卸载前判断 fail-closed、控制面展示链路)。槽位依赖按"删除该插件
// 后槽位是否仍有其它供给"精确判定——与 Remove 的 fail-closed 判定同一语义。
func (h *Host[C]) Dependents(pluginID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.plugins[pluginID]; !ok {
		return nil
	}
	slot := h.meta[pluginID].Slot
	var out []string
	for id, m := range h.meta {
		if id == pluginID {
			continue
		}
		if h.depBrokenByRemovalLocked(m, pluginID, slot) {
			out = append(out, id)
		}
	}
	return out
}

// CapabilityIndex 返回能力词 → 在该册插件声明者的反查表(元数据服务:
// 控制面/装配自检查"谁提供某能力")。
func (h *Host[C]) CapabilityIndex() map[string][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]string{}
	for id, m := range h.meta {
		for _, c := range m.Provides.Capabilities {
			out[c] = append(out[c], id)
		}
	}
	return out
}

// FnView 是函数目录的一行(元数据查询面)。
type FnView struct {
	PluginID string            `json:"plugin_id"`
	Spec     contract.FuncSpec `json:"spec"`
}

// Functions 返回全部在册函数目录(按 "plugin.fn" 键字典序)。
func (h *Host[C]) Functions() []FnView {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.fns))
	for k := range h.fns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]FnView, 0, len(keys))
	for _, k := range keys {
		e := h.fns[k]
		out = append(out, FnView{PluginID: e.pluginID, Spec: e.spec})
	}
	return out
}

func (h *Host[C]) AppliedConfig(pluginID string) (json.RawMessage, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.applied[pluginID]
	return v, ok
}

// EffectiveConfig 读插件**当前生效配置**:优先 ConfigProvider.Config()(权威,含默认/
// 归一化/脱敏),未实现或 panic 则回退宿主缓存 applied。这是 web 配置界面应读的"真值"。
func (h *Host[C]) EffectiveConfig(pluginID string) (json.RawMessage, bool) {
	h.mu.Lock()
	p, ok := h.plugins[pluginID]
	applied, has := h.applied[pluginID]
	h.mu.Unlock()
	if !ok {
		return nil, false
	}
	if cp, ok := p.(contract.ConfigProvider); ok {
		if cfg := safeConfig(cp); cfg != nil {
			return cfg, true
		}
	}
	return applied, has
}

// Snapshot 是宿主控制面全景快照(元数据/函数目录/能力索引/生效配置)。
type Snapshot struct {
	Plugins      []contract.Meta            `json:"plugins"`
	Functions    []FnView                   `json:"functions"`
	Capabilities map[string][]string        `json:"capabilities"`
	Applied      map[string]json.RawMessage `json:"applied_config"`
}

// Snapshot 输出当前宿主全景(纯净快照,不含实例句柄)。
func (h *Host[C]) Snapshot() Snapshot {
	h.mu.Lock()
	applied := make(map[string]json.RawMessage, len(h.applied))
	for k, v := range h.applied {
		applied[k] = v
	}
	h.mu.Unlock()
	return Snapshot{
		Plugins:      h.Plugins(),
		Functions:    h.Functions(),
		Capabilities: h.CapabilityIndex(),
		Applied:      applied,
	}
}
