// Package host 是插件宿主的**内容无关核心**（泛化 C=工具上下文类型）：
// 注册/卸载/热替换/函数目录/工具包装/配置 Schema 校验/槽位八轴+Needs 校验——
// 业务能力一律经装配注入(Op 扩展),文件/网络/日志等底层能力也由外部注入,词汇见契约包。
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/schema"
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

	pending        []string                   // 懒加载待补载 id(保持插入序)
	pendingPlugins map[string]contract.Plugin // id → 待补载插件
	netSchemes     map[string]bool            // 出站网络允许的协议 scheme(小写)
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

// Register 登记插件：DepInit 依赖、槽位八轴+Needs 校验、开点、函数目录。
func (h *Host[C]) Register(p contract.Plugin) (err error) {
	m := p.Meta()
	if m.ID == "" {
		return errors.New("host: empty id rejected")
	}
	surfaceAware, err := h.registerLocked(p, m)
	if err != nil {
		return err
	}
	h.mu.Lock()
	eff := h.effects[m.ID]
	h.mu.Unlock()
	// 能力门控 Surface 注入在锁外:插件 SetSurface 里可能回查宿主(Plugins/Call/…),
	// 若在 h.mu 内调用会死锁。
	if surfaceAware != nil {
		h.injectSurface(surfaceAware, m.ID, h.surfaceFor(m, eff))
	}
	h.logf("host: plugin %s registered (v%s)", m.ID, m.Version)
	h.emitLifecycle(contract.EventPluginRegistered, m.ID, m.Version)
	h.retryPending()
	return nil
}

// initDepsSatisfiedLocked 判定插件的 DepInit 依赖(点名或点槽位)是否已全部满足。
// 需持 h.mu。
func (h *Host[C]) initDepsSatisfiedLocked(m contract.Meta) bool {
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot) {
				return false
			}
			continue
		}
		if _, ok := h.plugins[r.Plugin]; !ok {
			return false
		}
		if r.MinVersion != "" && !versionAtLeast(h.meta[r.Plugin].Version, r.MinVersion) {
			return false
		}
	}
	return true
}

// versionAtLeast 极简语义化版本比较:按 "." 拆段逐段比数字(缺省段视 0)。非纯数字
// 段按字典序兜底。只用于 DepInit 依赖 MinVersion 的机器校验,非完整 semver。
func versionAtLeast(got, want string) bool {
	if want == "" {
		return true
	}
	g := strings.Split(got, ".")
	w := strings.Split(want, ".")
	for i := range w {
		gi := 0
		wi := 0
		if i < len(g) {
			gi, _ = strconv.Atoi(strings.TrimSpace(g[i]))
		}
		wi, _ = strconv.Atoi(strings.TrimSpace(w[i]))
		if gi != wi {
			return gi > wi
		}
	}
	return true
}

// RegisterStatus 是 RegisterLazy 的注册结果(显式区分"已加载 / 待补载")。
type RegisterStatus int

const (
	StatusRegistered RegisterStatus = iota // 已立即注册(依赖已满足)
	StatusPending                          // 依赖未满足,转入待补载队列
)

func (s RegisterStatus) String() string {
	if s == StatusPending {
		return "pending"
	}
	return "registered"
}

// RegisterLazy 登记插件:init 依赖已满足则立即注册(StatusRegistered);否则进待补载
// 队列(StatusPending),待后续 Register/RegisterMany/Replace 使依赖到位时自动补载。
// 仅空 id、与在册插件重复 id 才返回错误。重复懒登记同 id 会更新待补载实现。
func (h *Host[C]) RegisterLazy(p contract.Plugin) (RegisterStatus, error) {
	m := p.Meta()
	if m.ID == "" {
		return StatusRegistered, errors.New("host: empty id rejected")
	}
	h.mu.Lock()
	if _, ok := h.plugins[m.ID]; ok {
		h.mu.Unlock()
		return StatusRegistered, fmt.Errorf("host: plugin %s: duplicate id", m.ID)
	}
	if h.initDepsSatisfiedLocked(m) {
		h.mu.Unlock()
		return StatusRegistered, h.Register(p)
	}
	if _, ok := h.pendingPlugins[m.ID]; ok {
		// 已在待补载:替换成最新实现(幂等更新),仍返回 Pending。
		h.pendingPlugins[m.ID] = p
		h.mu.Unlock()
		return StatusPending, nil
	}
	h.pendingPlugins[m.ID] = p
	h.pending = append(h.pending, m.ID)
	h.mu.Unlock()
	h.logf("host: plugin %s deferred (init deps pending)", m.ID)
	return StatusPending, nil
}

// retryPending 宿主进册新插件后,重试待补载的懒插件:依赖到位即自动注册,仍缺
// 依赖者留在队列,其它硬失败(契约/重复等)记日志后丢弃。递归而不无限——每轮
// 至少移除一条"就绪"项,或原地终止。
func (h *Host[C]) retryPending() {
	h.mu.Lock()
	var ready []contract.Plugin
	kept := make([]string, 0, len(h.pending))
	for _, id := range h.pending {
		p := h.pendingPlugins[id]
		if h.initDepsSatisfiedLocked(p.Meta()) {
			ready = append(ready, p)
			delete(h.pendingPlugins, id)
		} else {
			kept = append(kept, id)
		}
	}
	h.pending = kept
	h.mu.Unlock()
	for _, p := range ready {
		if err := h.Register(p); err != nil {
			id := p.Meta().ID
			h.logf("host: deferred plugin %s failed: %v", id, err)
			h.emitPluginFailed(id, err)
		}
	}
}

// emitPluginFailed 广播懒插件补载硬失败(plugin.failed),供控制面观测。
func (h *Host[C]) emitPluginFailed(id string, err error) {
	if h.opts.Events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"plugin": id, "error": err.Error()})
	h.opts.Events.Dispatch(context.Background(), contract.Event{
		Type:    contract.EventPluginFailed,
		Version: contract.EnvelopeVersion,
		Source:  &contract.Origin{Kind: contract.OriginHost, Point: contract.EventPluginFailed, At: time.Now().UnixMilli()},
		Payload: payload,
	})
}

// removePendingLocked 从待补载队列移除指定 id(幂等;需持 h.mu)。
func (h *Host[C]) removePendingLocked(id string) {
	if _, ok := h.pendingPlugins[id]; !ok {
		return
	}
	delete(h.pendingPlugins, id)
	for i, pid := range h.pending {
		if pid == id {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			break
		}
	}
}

// registerLocked 在持锁下完成全部校验与簿记,返回 SurfaceAware 供锁外注入 Surface。
func (h *Host[C]) registerLocked(p contract.Plugin, m contract.Meta) (contract.SurfaceAware, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.plugins[m.ID]; ok {
		return nil, fmt.Errorf("host: plugin %s: duplicate id", m.ID)
	}
	// 该 id 若之前在待补载队列,进册/替换时同步移出,避免"pending + 已注册"双份。
	h.removePendingLocked(m.ID)
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot) {
				return nil, fmt.Errorf("host: plugin %s: init-dependency slot %q not satisfied", m.ID, r.Slot)
			}
			continue
		}
		if _, ok := h.plugins[r.Plugin]; !ok {
			return nil, fmt.Errorf("host: plugin %s: init-dependency %q not registered", m.ID, r.Plugin)
		}
		if r.MinVersion != "" && !versionAtLeast(h.meta[r.Plugin].Version, r.MinVersion) {
			return nil, fmt.Errorf("host: plugin %s: init-dependency %q version %q < required %q", m.ID, r.Plugin, h.meta[r.Plugin].Version, r.MinVersion)
		}
	}
	if m.Slot != "" {
		spec, ok, miss := h.slotCheck(m)
		if !ok {
			return nil, fmt.Errorf("host: plugin %s: unknown slot %q", m.ID, m.Slot)
		}
		if len(miss) > 0 {
			return nil, fmt.Errorf("host: plugin %s: slot %q minimal contract unmet: %s", m.ID, m.Slot, strings.Join(miss, "; "))
		}
		_ = spec
	}
	if fp, ok := p.(contract.FunctionProvider); ok {
		for _, f := range m.Provides.Functions {
			key := m.ID + "." + f.Name
			if _, exists := h.fns[key]; exists {
				return nil, fmt.Errorf("host: plugin %s: function %q conflicts", m.ID, f.Name)
			}
			h.fns[key] = fnEntry{pluginID: m.ID, spec: f, provider: fp}
		}
	}
	for _, hp := range m.Provides.Hooks {
		h.ensurePoint(contract.PointID(m.ID, hp.ID))
	}
	for _, pt := range m.Provides.Points {
		h.ensurePoint(pt)
	}
	for _, pt := range m.Requires.Listens {
		h.ensurePoint(pt)
	}
	for _, ev := range m.Provides.Events {
		if h.opts.Events != nil {
			h.opts.Events.EnsureTopic(contract.EventID(m.ID, ev.ID))
		}
	}
	h.plugins[m.ID] = p
	h.meta[m.ID] = m
	h.nextSeq++
	h.seq[m.ID] = h.nextSeq
	h.effects[m.ID] = &undo.Catcher{}
	var surfaceAware contract.SurfaceAware
	if ea, ok := p.(contract.SurfaceAware); ok {
		surfaceAware = ea
	}
	return surfaceAware, nil
}

func (h *Host[C]) ensurePoint(point string) {
	if h.opts.Points != nil {
		h.opts.Points.EnsurePoint(point)
	}
}

func (h *Host[C]) slotSatisfiedLocked(id string) bool {
	if h.opts.SlotLookup != nil {
		if s, ok := h.opts.SlotLookup(id); ok && s.Builtin {
			return true
		}
	}
	for _, m := range h.meta {
		if m.Slot == id {
			return true
		}
	}
	return false
}

// slotCheck 八轴求差 + Needs（Builtin 或任一在册主张者）。
func (h *Host[C]) slotCheck(m contract.Meta) (contract.SlotSpec, bool, []string) {
	spec, ok := contract.SlotSpec{}, false
	if h.opts.SlotLookup != nil {
		spec, ok = h.opts.SlotLookup(m.Slot)
	}
	if !ok {
		return spec, false, nil
	}
	var miss []string
	emit, capset, fnSet, hookSet, cfgSet, listen, events := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range m.Provides.Points {
		emit[v] = true
	}
	for _, v := range m.Provides.Capabilities {
		capset[v] = true
	}
	for _, f := range m.Provides.Functions {
		fnSet[f.Name] = true
	}
	for _, hk := range m.Provides.Hooks {
		hookSet[hk.ID] = true
	}
	for _, c := range m.Provides.Config {
		cfgSet[c.Key] = true
	}
	for _, ev := range m.Provides.Events {
		events[ev.ID] = true
	}
	for _, p := range m.Requires.Listens {
		listen[p] = true
	}
	for _, p := range spec.Provides {
		if !emit[p] {
			miss = append(miss, fmt.Sprintf("missing emitted point %q", p))
		}
	}
	for _, p := range spec.Capabilities {
		if !capset[p] {
			miss = append(miss, fmt.Sprintf("missing capability %q", p))
		}
	}
	for _, p := range spec.Functions {
		if !fnSet[p] {
			miss = append(miss, fmt.Sprintf("missing function %q", p))
		}
	}
	for _, p := range spec.Hooks {
		if !hookSet[p] {
			miss = append(miss, fmt.Sprintf("missing hook point %q", p))
		}
	}
	for _, p := range spec.Config {
		if !cfgSet[p] {
			miss = append(miss, fmt.Sprintf("missing config field %q", p))
		}
	}
	for _, p := range spec.Listens {
		if !listen[p] {
			miss = append(miss, fmt.Sprintf("missing listened point %q", p))
		}
	}
	for _, p := range spec.Events {
		if !events[p] {
			miss = append(miss, fmt.Sprintf("missing event topic %q", p))
		}
	}
	for _, n := range spec.Needs {
		if !h.slotSatisfiedLocked(n) {
			miss = append(miss, fmt.Sprintf("missing needed slot %q", n))
		}
	}
	// Tools 轴:槽位必备工具面 ⊇ 声明(Meta.NeedsTools)
	needTools := map[string]bool{}
	for _, t := range m.Requires.Tools {
		needTools[t] = true
	}
	for _, t := range spec.NeedsTools {
		if !needTools[t] {
			miss = append(miss, fmt.Sprintf("missing needed tool %q", t))
		}
	}
	// 框架就位校验:插件声明的 NeedsTools 必须有供给(任一在册插件提供或宿主注入)
	if h.opts.ToolNames != nil {
		available := map[string]bool{}
		for _, t := range h.opts.ToolNames() {
			available[t] = true
		}
		for _, t := range m.Requires.Tools {
			if !available[t] {
				miss = append(miss, fmt.Sprintf("tool %q not available in framework", t))
			}
		}
	}
	return spec, true, miss
}

// Remove 级联卸载（cascade=false 且被依赖时 fail-closed）。
// 只清宿主目录与 effect 栈;不调用 Disposer——若插件持有原生资源,调用方须自行
// Close 其句柄(autoload 卸载/热替换正是如此)。
func (h *Host[C]) Remove(pluginID string, cascade bool) ([]string, error) {
	h.mu.Lock()
	if _, ok := h.plugins[pluginID]; !ok {
		h.mu.Unlock()
		return nil, nil
	}
	victims := []string{pluginID}
	if !cascade {
		var deps []string
		for id, m := range h.meta {
			for _, r := range m.Requires.Deps {
				if r.Kind == contract.DepInit && (r.Plugin == pluginID || r.Slot == pluginID) {
					deps = append(deps, id)
				}
			}
		}
		if len(deps) > 0 {
			h.mu.Unlock()
			return nil, fmt.Errorf("host: remove %q refused: still required by %v (use cascade)", pluginID, deps)
		}
	}
	// 逐 victim 在删除前记录版本(供 removed 事件载荷)。
	versions := map[string]string{}
	queue := victims
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if m, ok := h.meta[id]; ok {
			versions[id] = m.Version
		}
		delete(h.plugins, id)
		delete(h.meta, id)
		delete(h.seq, id)
		for k := range h.fns {
			if h.fns[k].pluginID == id {
				delete(h.fns, k)
			}
		}
		delete(h.applied, id)
		if c := h.effects[id]; c != nil {
			c.Close()
			delete(h.effects, id)
		}
		for id2, m := range h.meta {
			for _, r := range m.Requires.Deps {
				if r.Kind == contract.DepInit && (r.Plugin == id || r.Slot == id) {
					victims = append(victims, id2)
					queue = append(queue, id2)
				}
			}
		}
	}
	h.mu.Unlock()

	h.logf("host: plugin %s removed (victims=%v)", pluginID, victims)
	// 生命周期事件须在锁外广播:订阅回调可能回查宿主,持锁会死锁。
	for _, v := range victims {
		h.emitLifecycle(contract.EventPluginRemoved, v, versions[v])
	}
	return victims, nil
}

// Replace 热替换同 id 插件。
// 只置换目录并清退旧实现的 effect 栈;旧实现若是 Disposer,调用方须自行 Close
// 旧句柄(autoload 热替换正是如此:Replace 后立即 old.Close)。
func (h *Host[C]) Replace(p contract.Plugin) error {
	id := p.Meta().ID
	var surfaceAware contract.SurfaceAware
	h.mu.Lock()
	if _, ok := h.plugins[id]; !ok {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %q not registered", id)
	}
	for k := range h.fns {
		if h.fns[k].pluginID == id {
			delete(h.fns, k)
		}
	}
	h.plugins[id] = p
	h.meta[id] = p.Meta()
	if fp, ok := p.(contract.FunctionProvider); ok {
		for _, f := range p.Meta().Provides.Functions {
			h.fns[id+"."+f.Name] = fnEntry{pluginID: id, spec: f, provider: fp}
		}
	}
	if ea, ok := p.(contract.SurfaceAware); ok {
		surfaceAware = ea
	}
	oldEffects := h.effects[id]
	h.effects[id] = &undo.Catcher{}
	newEff := h.effects[id]
	h.mu.Unlock()
	// 旧实现的 effect 撤销栈清退(订阅/hook 回调一并回滚)。
	if oldEffects != nil {
		oldEffects.Close()
	}
	// 同 Register:SetSurface 在锁外,插件可在其中回查宿主而不死锁。
	if surfaceAware != nil {
		h.injectSurface(surfaceAware, id, h.surfaceFor(p.Meta(), newEff))
	}
	h.logf("host: plugin %s replaced (v%s)", id, p.Meta().Version)
	h.emitLifecycle(contract.EventPluginReplaced, id, p.Meta().Version)
	h.retryPending()
	return nil
}

// Call 动态调用插件函数。默认对声明了 FuncSpec.Input/Output 的函数做 schema
// 校验(Options.DisableFuncValidation 关闭),入参不合规在调用前拒绝、返回不合规
// 在调用后拒绝。插件代码 panic 归一到 error(机制层 fail-closed)。
func (h *Host[C]) Call(ctx context.Context, pluginID, fname string, input json.RawMessage) (out json.RawMessage, err error) {
	defer fromPanic(&err, fmt.Sprintf("plugin %s function %q", pluginID, fname))
	// 无调用来源(宿主/应用直接发起)时,注入框架来源——被调函数经 OriginFrom 知道是框架在调,
	// 而非误判为"无调用者"。
	if contract.OriginFrom(ctx) == nil {
		ctx = contract.WithOrigin(ctx, &contract.Origin{Kind: contract.OriginHost, Point: fname, At: time.Now().UnixMilli()})
	}
	h.mu.Lock()
	e, ok := h.fns[pluginID+"."+fname]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("host: plugin %s: function %q not registered", pluginID, fname)
	}
	if !h.opts.DisableFuncValidation && len(e.spec.Input) > 0 {
		if issues := schema.Validate(e.spec.Input, input, "input"); len(issues) > 0 {
			err = fmt.Errorf("host: plugin %s: function %q: %s", pluginID, fname, strings.Join(issues, "; "))
		}
	}
	if err == nil {
		out, err = e.provider.CallFunc(ctx, fname, input)
	}
	if err == nil && !h.opts.DisableFuncValidation && len(e.spec.Output) > 0 {
		if issues := schema.Validate(e.spec.Output, out, "output"); len(issues) > 0 {
			err = fmt.Errorf("host: plugin %s: function %q: %s", pluginID, fname, strings.Join(issues, "; "))
			out = nil
		}
	}
	return out, err
}

// SetConfig 下发配置（配置 Schema 校验 + Configurable）。
// SetConfig 下发配置（配置 Schema 校验 + Configurable）。插件 ApplyConfig panic
// 归一到 error(机制层 fail-closed)。
func (h *Host[C]) SetConfig(pluginID string, cfg json.RawMessage) (err error) {
	defer fromPanic(&err, fmt.Sprintf("plugin %s apply config", pluginID))
	h.mu.Lock()
	p, ok := h.plugins[pluginID]
	m := h.meta[pluginID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("host: %q not registered", pluginID)
	}
	c, ok := p.(contract.Configurable)
	if !ok {
		return fmt.Errorf("host: plugin %s: not configurable", pluginID)
	}
	if len(m.Provides.Config) > 0 {
		pairs := make([]schema.ConfigPair, 0, len(m.Provides.Config))
		for _, f := range m.Provides.Config {
			if len(f.Schema) > 0 {
				pairs = append(pairs, schema.ConfigPair{Key: f.Key, Schema: f.Schema})
			}
		}
		if len(pairs) > 0 {
			if issues := schema.ValidateConfig(schema.BuildConfigSchema(pairs), cfg); len(issues) > 0 {
				return fmt.Errorf("host: plugin %s: config: %s", pluginID, strings.Join(issues, "; "))
			}
		}
	}
	if err := c.ApplyConfig(cfg); err != nil {
		return fmt.Errorf("host: plugin %s: %w", pluginID, err)
	}
	h.mu.Lock()
	// ApplyConfig 期间插件可能被替换/卸载:核对实例仍是同一个,防把旧实现的配置记到新 id 上。
	if h.plugins[pluginID] != p {
		h.mu.Unlock()
		return fmt.Errorf("host: plugin %s: replaced during config apply", pluginID)
	}
	h.applied[pluginID] = cfg
	h.mu.Unlock()
	// 配置生效观察事件(框架级;payload={plugin,config})
	if h.opts.Events != nil {
		payload, _ := json.Marshal(map[string]any{"plugin": pluginID, "config": cfg})
		h.opts.Events.Dispatch(context.Background(), contract.Event{
			Type:    contract.EventConfigUpdated,
			Version: contract.EnvelopeVersion,
			Source:  &contract.Origin{Kind: contract.OriginHost, At: time.Now().UnixMilli()},
			Payload: payload,
		})
	}
	return nil
}

// SurfaceFor 依插件 id 取能力门控 Surface 视图（远程/沙箱插件的能力回程路由入口）。
func (h *Host[C]) SurfaceFor(pluginID string) (contract.Surface, bool) {
	h.mu.Lock()
	p, ok := h.plugins[pluginID]
	if !ok {
		h.mu.Unlock()
		return nil, false
	}
	m := h.meta[pluginID]
	eff := h.effects[pluginID]
	if eff == nil {
		eff = &undo.Catcher{}
		h.effects[pluginID] = eff
	}
	h.mu.Unlock()
	return h.surfaceFor(m, eff), p != nil
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
// (元数据反查:卸载前判断 fail-closed、控制面展示链路)。
func (h *Host[C]) Dependents(pluginID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for id, m := range h.meta {
		for _, r := range m.Requires.Deps {
			if r.Kind == contract.DepInit && (r.Plugin == pluginID || r.Slot == pluginID) {
				out = append(out, id)
				break
			}
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

// SetConfigField 合并单字段进整份生效配置(读旧 applied→补 key→整对象下发)。egop 层
// 无能力门控(UI/装配层单字段保存用);与 SetConfig(整对象替换)区别开。
func (h *Host[C]) SetConfigField(pluginID, key string, value json.RawMessage) error {
	h.mu.Lock()
	raw, _ := h.applied[pluginID]
	h.mu.Unlock()
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &obj)
	}
	obj[key] = value
	full, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return h.SetConfig(pluginID, full)
}

// GetConfig 读某插件生效配置里的单个字段(egop 层读;无能力门控。跨插件读经
// Surface.GetConfig 施加 config.read + Readable 两层门控)。读的是 EffectiveConfig
// (ConfigProvider 优先,未实现回退 applied)。
func (h *Host[C]) GetConfig(pluginID, key string) (json.RawMessage, bool) {
	raw, ok := h.EffectiveConfig(pluginID)
	if !ok {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	v, found := obj[key]
	return v, found
}

// configFieldAllowed 查目标插件声明字段的跨插件访问标志(read=true 查 Readable,
// false 查 Writable);插件或字段未声明一律 false。
func (h *Host[C]) configFieldAllowed(pluginID, key string, read bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.meta[pluginID]
	if !ok {
		return false
	}
	for _, f := range m.Provides.Config {
		if f.Key == key {
			if read {
				return f.Readable
			}
			return f.Writable
		}
	}
	return false
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

// Tools 包装声明 CapTools 的 ToolProvider[C] 为 Tool 适配。
type Tool[C any] struct {
	Spec     contract.FuncSpec
	run      contract.ToolFunc[C]
	pluginID string
	host     *Host[C]
}

func (t *Tool[C]) Info() contract.FuncSpec { return t.Spec }

// Tools 收集全部插件工具。
func (h *Host[C]) Tools() []Tool[C] {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Tool[C]
	for id, p := range h.plugins {
		tp, ok := p.(contract.ToolProvider[C])
		if !ok || !contract.HasCapability(h.meta[id], contract.CapTools) {
			continue
		}
		// 工具面收集 best-effort:坏工具声明/生成 panic 跳过该插件,不 crash 宿主。
		func() {
			defer func() { _ = recover() }()
			for _, spec := range tp.ToolSpecs() {
				if fn, ok := tp.Tool(spec.Name); ok {
					out = append(out, Tool[C]{Spec: spec, run: fn, pluginID: id, host: h})
				}
			}
		}()
	}
	// 与 Plugins()/Functions() 一致:按 plugin.tool 键排序,输出确定(插件 map 遍历无序)。
	sort.Slice(out, func(i, j int) bool {
		return out[i].pluginID+"."+out[i].Spec.Name < out[j].pluginID+"."+out[j].Spec.Name
	})
	return out
}

// Run 执行工具（返回字符串形态,供模型面直用）。插件工具 panic 归一到 error。
func (t *Tool[C]) Run(ctx context.Context, tc *C, args json.RawMessage) (s string, err error) {
	defer fromPanic(&err, fmt.Sprintf("tool %q", t.Spec.Name))
	var out json.RawMessage
	out, err = t.run(ctx, tc, args)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// OnHook 注册 hook 回调(应用/装配层用;返回撤销函数)。
func (h *Host[C]) OnHook(hookID string, fn contract.HookFunc) func() {
	if h.opts.Hooks == nil {
		return func() {}
	}
	return h.opts.Hooks.On("", hookID, fn)
}

// TriggerHook 触发 hook 点:所有回调各返回一个 HookResult(按注册序汇总),
// 调用方据此判断是否有回调 Block=true 而阻断后续执行。
func (h *Host[C]) TriggerHook(ctx context.Context, hookID string, data json.RawMessage) []contract.HookResult {
	if h.opts.Hooks == nil {
		return nil
	}
	return h.opts.Hooks.Trigger(ctx, hookID, data)
}

// ---- Surface ----

type plugSurface[C any] struct {
	h       *Host[C]
	meta    contract.Meta
	caps    map[string]bool
	ops     map[string]Op
	effects *undo.Catcher
}

func (h *Host[C]) surfaceFor(m contract.Meta, eff *undo.Catcher) contract.Surface {
	caps := map[string]bool{}
	for _, c := range m.Provides.Capabilities {
		caps[c] = true
	}
	if eff == nil {
		eff = &undo.Catcher{}
	}
	return &plugSurface[C]{h: h, meta: m, caps: caps, ops: h.opts.Ops, effects: eff}
}

func (e *plugSurface[C]) Plugins() []contract.Meta {
	if !e.caps[contract.CapPluginMeta] {
		return nil
	}
	return e.h.Plugins()
}
func (e *plugSurface[C]) GetPlugin(id string) (contract.Meta, bool) {
	if !e.caps[contract.CapPluginMeta] {
		return contract.Meta{}, false
	}
	e.h.mu.Lock()
	m, ok := e.h.meta[id]
	e.h.mu.Unlock()
	return m, ok
}
func (e *plugSurface[C]) GetSetting(key string) (json.RawMessage, bool) {
	if e.h.opts.Settings == nil {
		return nil, false
	}
	return e.h.opts.Settings.Get(key)
}
func (e *plugSurface[C]) PublishEvent(ctx context.Context, topic string, payload json.RawMessage) {
	e.publish(ctx, contract.Event{Type: topic, Payload: payload})
}

// Publish 发布完整事件:调用方给 Type/SubType/Labels/Payload,框架回填 Version 与 Source。
func (e *plugSurface[C]) Publish(ctx context.Context, ev contract.Event) {
	e.publish(ctx, ev)
}

func (e *plugSurface[C]) publish(ctx context.Context, ev contract.Event) {
	if !e.caps[contract.CapEmitsEvents] || e.h.opts.Events == nil {
		return
	}
	if ev.Type == "" {
		return // 主题(topic)必填:无主题的事件无投递面,直接丢弃(fail-safe)。
	}
	ev.Version = contract.EnvelopeVersion
	ev.Source = &contract.Origin{
		ID:      e.meta.ID,
		Version: e.meta.Version,
		Kind:    contract.OriginEvent,
		Point:   ev.Type,
		At:      time.Now().UnixMilli(),
	}
	e.h.opts.Events.Dispatch(ctx, ev)
}
func (e *plugSurface[C]) Call(ctx context.Context, pluginID, fname string, input json.RawMessage) (json.RawMessage, error) {
	if !e.caps[contract.CapCallPlugins] {
		return nil, fmt.Errorf("plugin %s: capability %q not declared", e.meta.ID, contract.CapCallPlugins)
	}
	// 注入调用者来源,让被调函数经 contract.OriginFrom(ctx) 知道是谁调的自己。
	ctx = contract.WithOrigin(ctx, &contract.Origin{
		ID:      e.meta.ID,
		Version: e.meta.Version,
		Kind:    contract.OriginCall,
		Point:   fname,
		At:      time.Now().UnixMilli(),
	})
	return e.h.Call(ctx, pluginID, fname, input)
}
func (e *plugSurface[C]) SubscribeEvent(topic string, fn func(context.Context, string, contract.Event)) func() {
	return e.subscribe(&contract.EventFilter{Type: topic}, fn)
}

// SubscribeEventFilter 按过滤条件订阅(nil/零值 = 命中一切;字段间 AND 匹配)。
func (e *plugSurface[C]) SubscribeEventFilter(f *contract.EventFilter, fn func(context.Context, string, contract.Event)) func() {
	return e.subscribe(f, fn)
}

func (e *plugSurface[C]) subscribe(f *contract.EventFilter, fn func(context.Context, string, contract.Event)) func() {
	if !e.caps[contract.CapListensEvents] || e.h.opts.Events == nil {
		return func() {}
	}
	raw := e.h.opts.Events.Subscribe(f, func(ctx context.Context, ev contract.Event) {
		fn(ctx, ev.Type, ev)
	})
	// 同一撤销闭包可能被多处 Defer(进程内:宿主 effect 栈;远程:会话 cleanup
	// 的 UnsubAll 与宿主 effect 栈各记一次)。用 Once 包一层,重复调用只反注册一次,
	// 换非幂等的 Events 后端也不致双注销。
	var once sync.Once
	unsub := func() { once.Do(raw) }
	e.effects.Defer(unsub)
	return unsub
}

// OnHook 注册 hook 回调(插件侧;插件卸载/热替换时统一自动撤销)。
func (e *plugSurface[C]) OnHook(hookID string, fn contract.HookFunc) func() {
	if e.h.opts.Hooks == nil {
		return func() {}
	}
	unsub := e.h.opts.Hooks.On(e.meta.ID, hookID, fn)
	e.effects.Defer(unsub)
	return unsub
}
func (e *plugSurface[C]) Persist() (contract.FileStore, bool) {
	if !e.caps[contract.CapPersist] || e.h.opts.Storage == nil {
		return nil, false
	}
	if f := e.h.opts.Storage.File(e.meta.ID); f != nil {
		return f, true
	}
	return nil, false
}
func (e *plugSurface[C]) KV() (contract.KeyValue, bool) {
	if !e.caps[contract.CapKV] || e.h.opts.Storage == nil {
		return nil, false
	}
	if k := e.h.opts.Storage.KV(e.meta.ID); k != nil {
		return k, true
	}
	return nil, false
}
func (e *plugSurface[C]) Net() (contract.Net, bool) {
	if !e.caps[contract.CapNet] || e.h.opts.Net == nil {
		return nil, false
	}
	// 单点强制:权限门控(上面) + 协议门(下面)——目标必须是网络协议,拒绝 file:// 等。
	return netGuard{next: e.h.opts.Net, schemes: e.h.netSchemes}, true
}
func (e *plugSurface[C]) Exec(ctx context.Context, cmd string) (string, error) {
	if !e.caps[contract.CapExec] || e.h.opts.ExecFn == nil {
		return "", fmt.Errorf("plugin %s: capability %q not declared", e.meta.ID, contract.CapExec)
	}
	return e.h.opts.ExecFn(ctx, cmd)
}
func (e *plugSurface[C]) Op(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	capName := e.h.opts.opCap(name)
	if !e.caps[capName] {
		return nil, fmt.Errorf("plugin %s: capability %q not declared", e.meta.ID, capName)
	}
	fn, ok := e.ops[capName]
	if !ok {
		return nil, fmt.Errorf("plugin %s: capability %q not available", e.meta.ID, name)
	}
	return fn(ctx, input)
}

// GetConfig 读其它插件声明的配置字段:先 config.read 能力门控,再字段级 Readable。
func (e *plugSurface[C]) GetConfig(pluginID, key string) (json.RawMessage, bool) {
	if !e.caps[contract.CapConfigRead] || !e.h.configFieldAllowed(pluginID, key, true) {
		return nil, false
	}
	return e.h.GetConfig(pluginID, key)
}

// SetConfig 写其它插件声明的配置字段:先 config.write 能力门控,再字段级 Writable,
// 然后合并进整份生效配置走既有校验/下发/广播。
func (e *plugSurface[C]) SetConfig(pluginID, key string, value json.RawMessage) error {
	if !e.caps[contract.CapConfigWrite] {
		return fmt.Errorf("plugin %s: capability %q not declared", e.meta.ID, contract.CapConfigWrite)
	}
	if !e.h.configFieldAllowed(pluginID, key, false) {
		return fmt.Errorf("plugin %s: config field %s.%s not writable by plugins", e.meta.ID, pluginID, key)
	}
	return e.h.SetConfigField(pluginID, key, value)
}
