// 注册/替换/卸载生命周期:契约校验(DepInit 依赖/槽位八轴/名称与依赖形状)、
// 懒补载(pending)、Remove 级联与 Replace 热替换——Replace 与 Register 同款校验,
// 替换口不得比注册口宽松。
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/undo"
)

// Register 登记插件：DepInit 依赖、槽位八轴+Needs 校验、开点、函数目录。
// Meta 在此做**注册快照**(contract.CloneMeta 深拷贝):入册后插件侧改写自己的
// Meta 不影响宿主目录/校验/控制面视图;宿主外发(Plugins/Snapshot)共享冻结拷贝。
func (h *Host[C]) Register(p contract.Plugin) (err error) {
	m := contract.CloneMeta(p.Meta())
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

// hasBadNameChars 判定名字是否含空白/控制字符(rune ≤ ' ')。
func hasBadNameChars(s string) bool {
	for _, r := range s {
		if r <= ' ' {
			return true
		}
	}
	return false
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

// validateContractLocked 校验插件的契约前提:名称/依赖形状、DepInit 依赖已就位
// (点名/槽位/版本) + 槽位八轴最小契约满足。Register 与 Replace 共用——热替换
// 不得比首次注册宽松,否则坏包可绕过契约从替换口进入。需持 h.mu。
func (h *Host[C]) validateContractLocked(m contract.Meta) error {
	// 名称字符约束:插件 id 与函数名进入目录/函数键空间("id.fn"),注册口单点
	// fail-closed——"." 是键空间分隔符不允许出现在函数名(否则插件 a 的函数 "b.c"
	// 与插件 "a.b" 的函数 "c" 撞键),空白/控制字符一律拒(保留 id 含点:vendor.name
	// 命名惯例合法)。
	if hasBadNameChars(m.ID) {
		return fmt.Errorf("host: plugin %q: id must not contain whitespace or control characters", m.ID)
	}
	for _, f := range m.Provides.Functions {
		if f.Name == "" || strings.Contains(f.Name, ".") || hasBadNameChars(f.Name) {
			return fmt.Errorf("host: plugin %s: function name %q invalid (empty, contains '.' or whitespace)", m.ID, f.Name)
		}
	}
	for _, d := range m.Requires.Deps {
		if d.Plugin == "" && d.Slot == "" {
			return fmt.Errorf("host: plugin %s: dependency with neither plugin nor slot", m.ID)
		}
	}
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot) {
				return fmt.Errorf("host: plugin %s: init-dependency slot %q not satisfied", m.ID, r.Slot)
			}
			continue
		}
		if _, ok := h.plugins[r.Plugin]; !ok {
			return fmt.Errorf("host: plugin %s: init-dependency %q not registered", m.ID, r.Plugin)
		}
		if r.MinVersion != "" && !versionAtLeast(h.meta[r.Plugin].Version, r.MinVersion) {
			return fmt.Errorf("host: plugin %s: init-dependency %q version %q < required %q", m.ID, r.Plugin, h.meta[r.Plugin].Version, r.MinVersion)
		}
	}
	if m.Slot != "" {
		_, ok, miss := h.slotCheck(m)
		if !ok {
			return fmt.Errorf("host: plugin %s: unknown slot %q", m.ID, m.Slot)
		}
		if len(miss) > 0 {
			return fmt.Errorf("host: plugin %s: slot %q minimal contract unmet: %s", m.ID, m.Slot, strings.Join(miss, "; "))
		}
	}
	return nil
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
	// 替换语义下依赖校验以"自身已在册"为真:此处首注册,自身必不在册,无须排除。
	if err := h.validateContractLocked(m); err != nil {
		return nil, err
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
	h.rebuildHookDeclsLocked()
	return surfaceAware, nil
}

func (h *Host[C]) ensurePoint(point string) {
	if h.opts.Points != nil {
		h.opts.Points.EnsurePoint(point)
	}
}

// rebuildHookDeclsLocked 从在册 meta 全量重建 hook 点声明表(注册序=首声明者序)。
// 注册/替换/删除统一走重建:增量清理(删除/替换后哪些条目失主)的正确性成本
// 远高于一次全量折叠,注册路径低频,重建代价可忽略。需持 h.mu。
func (h *Host[C]) rebuildHookDeclsLocked() {
	type row struct {
		id  string
		seq uint64
		m   contract.Meta
	}
	rows := make([]row, 0, len(h.meta))
	for id, m := range h.meta {
		rows = append(rows, row{id: id, seq: h.seq[id], m: m})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	out := make(map[string]hookDecl, len(h.hookDecls))
	for _, r := range rows {
		for _, hp := range r.m.Provides.Hooks {
			if _, ok := out[hp.ID]; ok {
				continue // 首声明者胜(同名 hook 点多声明者并立时,以注册序取首)
			}
			kind := hp.Kind
			if kind == "" {
				kind = contract.KindModify // 缺省按 modify(可阻断)
			}
			out[hp.ID] = hookDecl{owner: r.id, kind: kind}
		}
	}
	h.hookDecls = out
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

// slotSatisfiedExcludingLocked 判定槽位在**排除某在册插件**后是否仍被满足
// (内置槽位,或该插件之外的任一在册主张者)。Remove 用它精确判定:被删插件
// 占据的槽位若仍有其它供给,槽位依赖方不算被破坏。需持 h.mu。
func (h *Host[C]) slotSatisfiedExcludingLocked(slot, excludeID string) bool {
	if h.opts.SlotLookup != nil {
		if s, ok := h.opts.SlotLookup(slot); ok && s.Builtin {
			return true
		}
	}
	for id, m := range h.meta {
		if id != excludeID && m.Slot == slot {
			return true
		}
	}
	return false
}

// depBrokenByRemovalLocked 判定在册插件 m 的 DepInit 依赖是否会因删除
// (removeID, 其占据槽位 removeSlot)而断:点名依赖直接断;点槽位依赖仅当该槽位
// 失去 removeID 后无任何其它供给(内置/其它主张者)才算断。需持 h.mu。
func (h *Host[C]) depBrokenByRemovalLocked(m contract.Meta, removeID, removeSlot string) bool {
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		switch {
		case r.Plugin != "":
			if r.Plugin == removeID {
				return true
			}
		case r.Slot != "" && r.Slot == removeSlot:
			if !h.slotSatisfiedExcludingLocked(r.Slot, removeID) {
				return true
			}
		}
	}
	return false
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
	targetSlot := h.meta[pluginID].Slot
	if !cascade {
		var deps []string
		for id, m := range h.meta {
			if id == pluginID {
				continue
			}
			if h.depBrokenByRemovalLocked(m, pluginID, targetSlot) {
				deps = append(deps, id)
			}
		}
		if len(deps) > 0 {
			h.mu.Unlock()
			return nil, fmt.Errorf("host: remove %q refused: still required by %v (use cascade)", pluginID, deps)
		}
	}
	// 逐 victim 在删除前记录版本(供 removed 事件载荷);inVictim 去重(一个依赖者
	// 可能有多条边指向被删集合,只计一次,removed 事件也只广播一次)。删除边删边判:
	// 槽位若仍有其它在册供给,槽位依赖方不进 victims。
	versions := map[string]string{}
	inVictim := map[string]bool{pluginID: true}
	victims := []string{pluginID}
	queue := victims
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if m, ok := h.meta[id]; ok {
			versions[id] = m.Version
		}
		idSlot := h.meta[id].Slot
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
			if inVictim[id2] {
				continue
			}
			if h.depBrokenByRemovalLocked(m, id, idSlot) {
				inVictim[id2] = true
				victims = append(victims, id2)
				queue = append(queue, id2)
			}
		}
	}
	h.rebuildHookDeclsLocked() // 声明者可能被删:全量重建
	h.mu.Unlock()

	h.logf("host: plugin %s removed (victims=%v)", pluginID, victims)
	// 生命周期事件须在锁外广播:订阅回调可能回查宿主,持锁会死锁。
	for _, v := range victims {
		h.emitLifecycle(contract.EventPluginRemoved, v, versions[v])
	}
	return victims, nil
}

// Replace 热替换同 id 插件。契约校验与 Register 同款(DepInit 依赖/槽位八轴)——
// 替换口不得比注册口宽松,坏包 fail-closed 拒换、旧版继续服务。
// 只置换目录并清退旧实现的 effect 栈;旧实现若是 Disposer,调用方须自行 Close
// 旧句柄(autoload 热替换正是如此:Replace 后立即 old.Close)。
func (h *Host[C]) Replace(p contract.Plugin) error {
	m := contract.CloneMeta(p.Meta()) // 同 Register:替换件入册即快照
	id := m.ID
	var surfaceAware contract.SurfaceAware
	h.mu.Lock()
	if _, ok := h.plugins[id]; !ok {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %q not registered", id)
	}
	if err := h.validateContractLocked(m); err != nil {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %w", err)
	}
	for k := range h.fns {
		if h.fns[k].pluginID == id {
			delete(h.fns, k)
		}
	}
	h.plugins[id] = p
	h.meta[id] = m
	appliedCfg, hasApplied := h.applied[id] // 持锁顺带取(applied 由 h.mu 保护)
	// 替换件的新声明面(点位/主题)与首注册一致地补落。
	for _, hp := range m.Provides.Hooks {
		h.ensurePoint(contract.PointID(id, hp.ID))
	}
	for _, pt := range m.Provides.Points {
		h.ensurePoint(pt)
	}
	for _, pt := range m.Requires.Listens {
		h.ensurePoint(pt)
	}
	for _, ev := range m.Provides.Events {
		if h.opts.Events != nil {
			h.opts.Events.EnsureTopic(contract.EventID(id, ev.ID))
		}
	}
	if fp, ok := p.(contract.FunctionProvider); ok {
		for _, f := range m.Provides.Functions {
			h.fns[id+"."+f.Name] = fnEntry{pluginID: id, spec: f, provider: fp}
		}
	}
	if ea, ok := p.(contract.SurfaceAware); ok {
		surfaceAware = ea
	}
	oldEffects := h.effects[id]
	h.effects[id] = &undo.Catcher{}
	newEff := h.effects[id]
	h.rebuildHookDeclsLocked() // 新版本可能增删自有 hook 点:全量重建
	h.mu.Unlock()
	// 旧实现的 effect 撤销栈清退(订阅/hook 回调一并回滚)。
	if oldEffects != nil {
		oldEffects.Close()
	}
	// 同 Register:SetSurface 在锁外,插件可在其中回查宿主而不死锁。
	if surfaceAware != nil {
		h.injectSurface(surfaceAware, id, h.surfaceFor(m, newEff))
	}
	// 替换件配置回灌:applied 缓存是宿主侧状态,新实例必须继承——否则热更
	// 静默丢已配置行为(如审批开关回落默认关=fail-open)。与装配序一致:
	// 先 SetSurface 再 ApplyConfig;「替换口不得比注册口宽松」同源精神。
	// 直接 ApplyConfig 而非 SetConfig:本方法可能在 SetConfig→ApplyConfig→
	// Replace(自我替换)链上被调,再取 cfgMu 即自死锁;schema 已在原 SetConfig
	// 校验过、applied 缓存无需重写,panic 经 fromPanic 归一(机制层故障隔离)。
	if hasApplied {
		if c, ok := p.(contract.Configurable); ok {
			var reapplied error
			func() {
				defer fromPanic(&reapplied, fmt.Sprintf("plugin %s re-apply config", id))
				reapplied = c.ApplyConfig(appliedCfg)
			}()
			if reapplied != nil {
				h.logf("host: plugin %s replaced but config re-apply failed: %v", id, reapplied)
			}
		}
	}
	h.logf("host: plugin %s replaced (v%s)", id, m.Version)
	h.emitLifecycle(contract.EventPluginReplaced, id, m.Version)
	h.retryPending()
	return nil
}
