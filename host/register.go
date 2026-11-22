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
// Plugin/Slot 双设时统一采用 Slot 优先（Register/Replace/Remove/RegisterMany
// 同规则）；Slot 依赖不使用 MinVersion。需持 h.mu；prospective 用于 Replace
// 在拟提交目录上预检，省略时读当前 h.meta。
func (h *Host[C]) initDepsSatisfiedLocked(m contract.Meta, prospective ...map[string]contract.Meta) bool {
	registry := h.meta
	if len(prospective) > 0 {
		registry = prospective[0]
	}
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot, registry) {
				return false
			}
			continue
		}
		dependency, ok := registry[r.Plugin]
		if !ok {
			return false
		}
		if r.MinVersion != "" && !versionAtLeast(dependency.Version, r.MinVersion) {
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

// parseNumericVersion 解析点分十进制版本。每段都必须是非空纯数字;
// 溢出、空段和其它字符一律报错,不能静默当成零参与依赖比较。
func parseNumericVersion(s string) ([]int, error) {
	if s == "" {
		return nil, errors.New("empty version")
	}
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("version segment %d is empty", i+1)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("version segment %q is not numeric", part)
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("version segment %q: %w", part, err)
		}
		out[i] = n
	}
	return out, nil
}

// versionAtLeast 极简语义化版本比较:按 "." 拆段逐段比数字(缺省段视 0)。
// 任一侧含非法段时比较失败;MinVersion 自身还会由 validateMinVersionConstraints
// 返回带上下文的注册错误。只用于 DepInit 依赖,非完整 semver。
func versionAtLeast(got, want string) bool {
	if want == "" {
		return true
	}
	g, err := parseNumericVersion(got)
	if err != nil {
		return false
	}
	w, err := parseNumericVersion(want)
	if err != nil {
		return false
	}
	for i := range w {
		gi := 0
		if i < len(g) {
			gi = g[i]
		}
		if gi != w[i] {
			return gi > w[i]
		}
	}
	return true
}

func validateMinVersionConstraints(m contract.Meta) error {
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit || r.Slot != "" || r.MinVersion == "" {
			continue
		}
		if _, err := parseNumericVersion(r.MinVersion); err != nil {
			return fmt.Errorf("host: plugin %s: invalid min_version %q for init-dependency %q: %w", m.ID, r.MinVersion, r.Plugin, err)
		}
	}
	return nil
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
// 空 id、非法 MinVersion、与在册插件重复 id 返回错误。重复懒登记同 id 会更新待补载实现。
func (h *Host[C]) RegisterLazy(p contract.Plugin) (RegisterStatus, error) {
	m := contract.CloneMeta(p.Meta())
	if m.ID == "" {
		return StatusRegistered, errors.New("host: empty id rejected")
	}
	if err := validateMinVersionConstraints(m); err != nil {
		return StatusRegistered, err
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
		// 已在待补载:替换成最新实现与不可变 Meta 快照(幂等更新),仍返回 Pending。
		h.pendingPlugins[m.ID] = p
		h.pendingMeta[m.ID] = m
		h.mu.Unlock()
		return StatusPending, nil
	}
	h.pendingPlugins[m.ID] = p
	h.pendingMeta[m.ID] = m
	h.pending = append(h.pending, m.ID)
	h.mu.Unlock()
	h.logf("host: plugin %s deferred (init deps pending)", m.ID)
	return StatusPending, nil
}

// retryPending 重试待补载插件。锁内只读 pendingPlugins 与 RegisterLazy 保存的
// CloneMeta 快照，绝不调用外部 Meta；Register 成功提交时才移除 pending。
// 硬失败保留 pending，后续依赖/目录变化仍可重试，且不会遗留函数目录半成品。
func (h *Host[C]) retryPending() {
	type readyPlugin struct {
		p  contract.Plugin
		id string
	}
	h.mu.Lock()
	var ready []readyPlugin
	for _, id := range h.pending {
		p, ok := h.pendingPlugins[id]
		if !ok {
			continue
		}
		m := h.pendingMeta[id]
		if h.initDepsSatisfiedLocked(m) {
			ready = append(ready, readyPlugin{p: p, id: id})
		}
	}
	h.mu.Unlock()
	for _, r := range ready {
		if err := h.Register(r.p); err != nil {
			h.logf("host: deferred plugin %s failed: %v", r.id, err)
			h.emitPluginFailed(r.id, err)
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
	delete(h.pendingMeta, id)
	for i, pid := range h.pending {
		if pid == id {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			break
		}
	}
}

// validateContractLocked 校验名称/依赖形状、DepInit(点名/槽位/版本)与槽位
// 八轴最小契约。prospective 非空时在拟提交 Meta 目录上校验；当前与拟提交
// 目录共用同一 Slot 优先规则。需持 h.mu。
func (h *Host[C]) validateContractLocked(m contract.Meta, prospective ...map[string]contract.Meta) error {
	registry := h.meta
	if len(prospective) > 0 {
		registry = prospective[0]
	}
	if err := validateMinVersionConstraints(m); err != nil {
		return err
	}
	// 名称字符约束:插件 id 与函数名进入目录/函数键空间("id.fn")。
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
		// 双设依赖统一 Slot 优先；MinVersion 只属于点名插件依赖。
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot, registry) {
				return fmt.Errorf("host: plugin %s: init-dependency slot %q not satisfied", m.ID, r.Slot)
			}
			continue
		}
		dependency, ok := registry[r.Plugin]
		if !ok {
			return fmt.Errorf("host: plugin %s: init-dependency %q not registered", m.ID, r.Plugin)
		}
		if r.MinVersion != "" && !versionAtLeast(dependency.Version, r.MinVersion) {
			return fmt.Errorf("host: plugin %s: init-dependency %q version %q < required %q", m.ID, r.Plugin, dependency.Version, r.MinVersion)
		}
	}
	if m.Slot != "" {
		_, ok, miss := h.slotCheck(m, registry)
		if !ok {
			return fmt.Errorf("host: plugin %s: unknown slot %q", m.ID, m.Slot)
		}
		if len(miss) > 0 {
			return fmt.Errorf("host: plugin %s: slot %q minimal contract unmet: %s", m.ID, m.Slot, strings.Join(miss, "; "))
		}
	}
	return nil
}

// validateProspectiveReplaceLocked 在“id 已被新版本占据”的完整拟提交目录
// 上重验所有在册插件，而不仅是替换件自身。这样版本降级、Slot 撤离/变化、
// SlotSpec.Needs 失联等会让任意既有依赖悬空的热更都会在 prepare 前被拒绝。
// ValidateProspective 在“候选清单已占据其 id”的完整拟提交目录上重验依赖与
// 槽位契约，不实例化插件，也不改变当前注册表。新装与同 id 替换共用同一入口。
func (h *Host[C]) ValidateProspective(m contract.Meta) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validateProspectiveReplaceLocked(m.ID, contract.CloneMeta(m))
}

func (h *Host[C]) validateProspectiveReplaceLocked(id string, m contract.Meta) error {
	prospective := make(map[string]contract.Meta, len(h.meta))
	for existingID, existingMeta := range h.meta {
		prospective[existingID] = existingMeta
	}
	prospective[id] = m
	for existingID, existingMeta := range prospective {
		if err := h.validateContractLocked(existingMeta, prospective); err != nil {
			if existingID == id {
				return err
			}
			return fmt.Errorf("replacement would invalidate existing plugin %s: %w", existingID, err)
		}
	}
	return nil
}

// stageFunctionsLocked 预建完整函数目录增量。所有冲突/重复都在写入 h.fns
// 前发现；replacing=true 时仅允许覆盖同 id 旧代条目。需持 h.mu。
func (h *Host[C]) stageFunctionsLocked(id string, m contract.Meta, fp contract.FunctionProvider, g *pluginGeneration, replacing bool) (map[string]fnEntry, error) {
	staged := map[string]fnEntry{}
	if fp == nil {
		return staged, nil
	}
	for _, f := range m.Provides.Functions {
		key := id + "." + f.Name
		if _, duplicate := staged[key]; duplicate {
			return nil, fmt.Errorf("host: plugin %s: function %q conflicts", id, f.Name)
		}
		if existing, exists := h.fns[key]; exists && (!replacing || existing.pluginID != id) {
			return nil, fmt.Errorf("host: plugin %s: function %q conflicts", id, f.Name)
		}
		staged[key] = fnEntry{pluginID: id, spec: f, provider: fp, generation: g}
	}
	return staged, nil
}

// registerLocked 在持锁下完成全部校验与簿记,返回 SurfaceAware 供锁外注入 Surface。
// pending 移除和函数目录写入都发生在所有校验成功之后；staged 函数一次性提交，
// 任何错误路径均不会留下 ghost function。
func (h *Host[C]) registerLocked(p contract.Plugin, m contract.Meta) (contract.SurfaceAware, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.plugins[m.ID]; ok {
		return nil, fmt.Errorf("host: plugin %s: duplicate id", m.ID)
	}
	if err := h.validateContractLocked(m); err != nil {
		return nil, err
	}
	g := h.newGenerationLocked(m.ID)
	fp, _ := p.(contract.FunctionProvider)
	staged, err := h.stageFunctionsLocked(m.ID, m, fp, g, false)
	if err != nil {
		return nil, err
	}
	// 只有全部校验与函数预检成功后才允许移出 pending。
	h.removePendingLocked(m.ID)
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
	for key, entry := range staged {
		h.fns[key] = entry
	}
	h.plugins[m.ID] = p
	h.meta[m.ID] = m
	h.generations[m.ID] = g
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

func (h *Host[C]) slotSatisfiedLocked(id string, prospective ...map[string]contract.Meta) bool {
	registry := h.meta
	if len(prospective) > 0 {
		registry = prospective[0]
	}
	if h.opts.SlotLookup != nil {
		if s, ok := h.opts.SlotLookup(id); ok && s.Builtin {
			return true
		}
	}
	for _, m := range registry {
		if m.Slot == id {
			return true
		}
	}
	return false
}

// slotCheck 八轴求差 + Needs（Builtin 或任一在册主张者）。prospective 与
// validateContractLocked 共用同一拟提交目录。
func (h *Host[C]) slotCheck(m contract.Meta, prospective ...map[string]contract.Meta) (contract.SlotSpec, bool, []string) {
	registry := h.meta
	if len(prospective) > 0 {
		registry = prospective[0]
	}
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
		if !h.slotSatisfiedLocked(n, registry) {
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

// slotSatisfiedAfterRemovalLocked 判定槽位在拟删除集合之外是否仍有供给。
func (h *Host[C]) slotSatisfiedAfterRemovalLocked(slot string, removed map[string]bool) bool {
	if h.opts.SlotLookup != nil {
		if s, ok := h.opts.SlotLookup(slot); ok && s.Builtin {
			return true
		}
	}
	for id, m := range h.meta {
		if !removed[id] && m.Slot == slot {
			return true
		}
	}
	return false
}

// depBrokenByRemovalSetLocked 在完整拟删除集合上判定 DepInit 是否悬空。
// 双设依赖与注册/批量排序一致采用 Slot 优先，不能因 Plugin 指向无关 id
// 而漏掉 Slot 边的破坏。
func (h *Host[C]) depBrokenByRemovalSetLocked(m contract.Meta, removed map[string]bool) bool {
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedAfterRemovalLocked(r.Slot, removed) {
				return true
			}
			continue
		}
		if removed[r.Plugin] {
			return true
		}
	}
	return false
}

func (h *Host[C]) depBrokenByRemovalLocked(m contract.Meta, removeID string) bool {
	return h.depBrokenByRemovalSetLocked(m, map[string]bool{removeID: true})
}

// Remove 级联卸载（cascade=false 且被依赖时 fail-closed）。
// 只清宿主目录与 effect 栈;不调用 Disposer。旧代先禁止新调用再排空；
// 超时/取消不提交卸载，旧目录与 effect 保留。带 ctx 的取消入口见 RemoveContext。
func (h *Host[C]) Remove(pluginID string, cascade bool) ([]string, error) {
	return h.RemoveContext(context.Background(), pluginID, cascade)
}

// RemoveContext 是 Remove 的可取消版本；等待上限取 Options.RemoveQuiesce。
func (h *Host[C]) RemoveContext(ctx context.Context, pluginID string, cascade bool) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	h.lifecycleMu.Lock()
	lifecycleLocked := true
	unlockLifecycle := func() {
		if lifecycleLocked {
			h.lifecycleMu.Unlock()
			lifecycleLocked = false
		}
	}
	defer unlockLifecycle()

	h.mu.Lock()
	if _, ok := h.plugins[pluginID]; !ok {
		h.mu.Unlock()
		return nil, nil
	}
	removed := map[string]bool{pluginID: true}
	victims := []string{pluginID}
	if !cascade {
		var deps []string
		for id, m := range h.meta {
			if id != pluginID && h.depBrokenByRemovalSetLocked(m, removed) {
				deps = append(deps, id)
			}
		}
		if len(deps) > 0 {
			h.mu.Unlock()
			return nil, fmt.Errorf("host: remove %q refused: still required by %v (use cascade)", pluginID, deps)
		}
	} else {
		// 在完整拟删除集合上规划 victims，不边删边改目录。任一后续静默失败
		// 都可无残留地放弃整个事务。
		for processed := 0; processed < len(victims); processed++ {
			for candidateID, m := range h.meta {
				if removed[candidateID] {
					continue
				}
				if h.depBrokenByRemovalSetLocked(m, removed) {
					removed[candidateID] = true
					victims = append(victims, candidateID)
				}
			}
		}
	}
	versions := map[string]string{}
	type removalRow struct {
		id     string
		p      contract.Plugin
		g      *pluginGeneration
		effect *undo.Catcher
	}
	rows := make([]removalRow, 0, len(victims))
	for _, id := range victims {
		versions[id] = h.meta[id].Version
		g := h.generations[id]
		h.sealGenerationLocked(g)
		rows = append(rows, removalRow{id: id, p: h.plugins[id], g: g, effect: h.effects[id]})
	}
	h.mu.Unlock()

	// 同一父 deadline 覆盖整个级联排空过程，避免 victims 数量放大等待上限。
	qctx := ctx
	cancel := func() {}
	if wait := h.opts.RemoveQuiesce; wait > 0 {
		qctx, cancel = context.WithTimeout(ctx, wait)
	}
	defer cancel()
	for _, row := range rows {
		if err := h.awaitGenerationQuiesce(qctx, row.id, row.g, row.p, h.opts.RemoveQuiesce); err != nil {
			h.mu.Lock()
			for _, pending := range rows {
				h.resumeGenerationLocked(pending.g)
			}
			h.mu.Unlock()
			return nil, fmt.Errorf("host: remove %q: %w", pluginID, err)
		}
	}

	// 所有旧代已排空后才原子删除目录；effect 一律锁外清退，允许撤销函数
	// 回查 Host 而不会与 h.mu 自死锁。
	h.mu.Lock()
	var effects []*undo.Catcher
	for _, row := range rows {
		id := row.id
		delete(h.plugins, id)
		delete(h.meta, id)
		delete(h.seq, id)
		delete(h.generations, id)
		for k := range h.fns {
			if h.fns[k].pluginID == id {
				delete(h.fns, k)
			}
		}
		delete(h.applied, id)
		if row.effect != nil {
			effects = append(effects, row.effect)
		}
		delete(h.effects, id)
	}
	h.rebuildHookDeclsLocked()
	h.mu.Unlock()
	unlockLifecycle()
	for _, effect := range effects {
		if err := effect.Close(); err != nil {
			h.logf("host: remove effect cleanup: %v", err)
		}
	}

	h.logf("host: plugin %s removed (victims=%v)", pluginID, victims)
	for _, v := range victims {
		h.emitLifecycle(contract.EventPluginRemoved, v, versions[v])
	}
	return victims, nil
}

// Replace 热替换同 id 插件。流程为 prospective 校验→prepare(能力面/配置回灌)
// →封闭旧代准入→排空→原子 commit→清退旧 effect。任一步失败均不广播
// plugin.replaced，旧实例/旧目录/旧 effect 保持服务。可取消入口见 ReplaceContext。
func (h *Host[C]) Replace(p contract.Plugin) error {
	return h.ReplaceContext(context.Background(), p)
}

// ReplaceContext 是 Replace 的可取消版本；静默等待上限取 Options.ReplaceQuiesce。
func (h *Host[C]) ReplaceContext(ctx context.Context, p contract.Plugin) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m := contract.CloneMeta(p.Meta()) // 外部 Meta 在 lifecycleMu/h.mu 之外执行
	id := m.ID
	lifecycleLocked := false
	unlockLifecycle := func() {
		if lifecycleLocked {
			h.lifecycleMu.Unlock()
			lifecycleLocked = false
		}
	}
	defer unlockLifecycle()

	h.mu.Lock()
	old, ok := h.plugins[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %q not registered", id)
	}
	if err := h.validateProspectiveReplaceLocked(id, m); err != nil {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %w", err)
	}
	oldGeneration := h.generations[id]
	oldEffects := h.effects[id]
	appliedCfg, hasApplied := h.applied[id]
	appliedCfg = append(json.RawMessage(nil), appliedCfg...)
	newGeneration := h.newGenerationLocked(id)
	fp, _ := p.(contract.FunctionProvider)
	staged, err := h.stageFunctionsLocked(id, m, fp, newGeneration, true)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("host: replace: %w", err)
	}
	h.mu.Unlock()

	// 配置先在旧目录仍有效时完成 Schema 重验与 prepare；失败不触碰旧 effect。
	var configurable contract.Configurable
	if hasApplied {
		var ok bool
		configurable, ok = p.(contract.Configurable)
		if !ok {
			return fmt.Errorf("host: replace: plugin %s: existing config cannot be replayed: new instance is not configurable", id)
		}
		if err := validateConfigForMeta(id, m, appliedCfg); err != nil {
			return fmt.Errorf("host: replace: config replay: %w", err)
		}
	}
	newEffects := &undo.Catcher{}
	closePrepared := func() {
		unlockLifecycle()
		if err := newEffects.Close(); err != nil {
			h.logf("host: plugin %s prepared effect cleanup: %v", id, err)
		}
	}
	if surfaceAware, ok := p.(contract.SurfaceAware); ok {
		h.injectSurface(surfaceAware, id, h.surfaceFor(m, newEffects))
	}
	if hasApplied {
		// 不取 cfgMu：Replace 可能由 SetConfig→ApplyConfig→Replace 自替换链进入。
		// Schema 已按新 Meta 重验；ApplyConfig panic 统一归一为错误。
		var reapplied error
		func() {
			defer fromPanic(&reapplied, fmt.Sprintf("plugin %s re-apply config", id))
			reapplied = configurable.ApplyConfig(appliedCfg)
		}()
		if reapplied != nil {
			closePrepared()
			return fmt.Errorf("host: replace: plugin %s config re-apply failed: %w", id, reapplied)
		}
	}

	// prepare 不持 lifecycleMu，SetSurface/ApplyConfig 可安全回查宿主。进入
	// seal→drain→commit 串行区后重新校验 prospective registry 与函数冲突，
	// 防止 prepare 期间其它插件的注册/替换使先前判断过期。
	h.lifecycleMu.Lock()
	lifecycleLocked = true
	h.mu.Lock()
	if h.generations[id] != oldGeneration || h.plugins[id] != old {
		h.mu.Unlock()
		closePrepared()
		return fmt.Errorf("host: replace: plugin %s changed during prepare", id)
	}
	if err := h.validateProspectiveReplaceLocked(id, m); err != nil {
		h.mu.Unlock()
		closePrepared()
		return fmt.Errorf("host: replace: registry changed during prepare: %w", err)
	}
	staged, err = h.stageFunctionsLocked(id, m, fp, newGeneration, true)
	if err != nil {
		h.mu.Unlock()
		closePrepared()
		return fmt.Errorf("host: replace: %w", err)
	}
	h.sealGenerationLocked(oldGeneration)
	h.mu.Unlock()
	if err := h.awaitGenerationQuiesce(ctx, id, oldGeneration, old, h.opts.ReplaceQuiesce); err != nil {
		h.mu.Lock()
		h.resumeGenerationLocked(oldGeneration)
		h.mu.Unlock()
		closePrepared()
		return fmt.Errorf("host: replace: %w", err)
	}

	// 只有旧代真正排空后才提交函数/实例/Meta/effect 目录。声明点位也在
	// commit 才补落，失败路径不会留下新声明面。
	h.mu.Lock()
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
	for k := range h.fns {
		if h.fns[k].pluginID == id {
			delete(h.fns, k)
		}
	}
	for key, entry := range staged {
		h.fns[key] = entry
	}
	h.plugins[id] = p
	h.meta[id] = m
	h.generations[id] = newGeneration
	h.effects[id] = newEffects
	h.rebuildHookDeclsLocked()
	h.mu.Unlock()
	unlockLifecycle()

	// 提交成功后旧 effect 才锁外清退。新实例 prepare 可能已产生外部副作用；
	// effect 只能撤销经 Surface 登记的订阅/hook 等，宿主不声称能回滚网络、
	// 文件或其它外部副作用。调用方仍负责旧 Disposer（autoload 在成功后 Close）。
	if oldEffects != nil {
		if err := oldEffects.Close(); err != nil {
			h.logf("host: plugin %s old effect cleanup: %v", id, err)
		}
	}
	h.logf("host: plugin %s replaced (v%s)", id, m.Version)
	h.emitLifecycle(contract.EventPluginReplaced, id, m.Version)
	h.retryPending()
	return nil
}
