// 批量注册:分析 DepInit 依赖做拓扑排序后依序注册,失败隔离。
package host

import (
	"fmt"
	"strings"

	"github.com/ejfkdev/egop/contract"
)

// RegisterFailure 描述一个批量注册中未注册的插件及原因。
type RegisterFailure struct {
	ID  string
	Err error
}

// RegisterReport 是 RegisterMany 的结果。
type RegisterReport struct {
	// Registered 是成功注册的插件 id,按最终(拓扑)注册顺序。
	Registered []string
	// Pending 是依赖尚未满足、转入待补载队列的插件 id(依赖 pending 插件时)。
	Pending []string
	// Failed 是未注册的插件及原因(缺依赖/成环/重复 id/槽位或函数契约不满足等)。
	Failed []RegisterFailure
}

// depKey 归一化依赖键:p:<id> 点名插件,s:<name> 点到槽位面。
func pluginKey(id string) string { return "p:" + id }
func slotKey(name string) string { return "s:" + name }

// needKeys 收集 Meta 的 DepInit 依赖键(只排序硬依赖;call/soft 不参与装载序)。
func needKeys(m contract.Meta) []string {
	var out []string
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		switch {
		case r.Plugin != "":
			out = append(out, pluginKey(r.Plugin))
		case r.Slot != "":
			out = append(out, slotKey(r.Slot))
		}
	}
	return out
}

// provideKeys 收集 Meta 能供给的依赖键(自身 id + 声称的槽位)。
func provideKeys(m contract.Meta) []string {
	out := []string{pluginKey(m.ID)}
	if m.Slot != "" {
		out = append(out, slotKey(m.Slot))
	}
	return out
}

// keyProvided 判断一个依赖键是否已被"外部"(已在册插件/内置槽位)满足。
func (h *Host[C]) keyProvided(key string, registered, registeredSlot map[string]bool) bool {
	if id, ok := strings.CutPrefix(key, "p:"); ok {
		return registered[id]
	}
	if name, ok := strings.CutPrefix(key, "s:"); ok {
		if registeredSlot[name] {
			return true
		}
		if h.opts.SlotLookup != nil {
			if spec, ok := h.opts.SlotLookup(name); ok && spec.Builtin {
				return true
			}
		}
	}
	return false
}

func prettyKey(key string) string {
	if id, ok := strings.CutPrefix(key, "p:"); ok {
		return fmt.Sprintf("plugin %q", id)
	}
	if name, ok := strings.CutPrefix(key, "s:"); ok {
		return fmt.Sprintf("slot %q", name)
	}
	return key
}

func hasActiveProvider(providers []int, active []bool) bool {
	for _, i := range providers {
		if active[i] {
			return true
		}
	}
	return false
}

// RegisterMany 批量注册:分析 DepInit 依赖(Requires 的 Plugin/Slot)做拓扑排序,
// 再依序注册。失败隔离:缺依赖、成环、重复 id、槽位/函数契约不满足的插件单独
// 记入 Failed,不阻断其余。可与 wasm.ScanFS 返回的 []*Plugin 直接衔接。
func (h *Host[C]) RegisterMany(plugs []contract.Plugin) RegisterReport {
	var rep RegisterReport

	// 1) 快照外部已满足面(已在册插件及其槽位)+ 待补载插件(判定"依赖 pending"应转为待补载而非失败)。
	h.mu.Lock()
	registered := map[string]bool{}
	registeredSlot := map[string]bool{}
	for id, m := range h.meta {
		registered[id] = true
		if m.Slot != "" {
			registeredSlot[m.Slot] = true
		}
	}
	// eventually 收"最终会到位的依赖键"——待补载插件提供的 id/槽位,以及本批被转为待补载的条目。
	eventually := map[string]bool{}
	for id, p := range h.pendingPlugins {
		eventually[pluginKey(id)] = true
		if s := p.Meta().Slot; s != "" {
			eventually[slotKey(s)] = true
		}
	}
	h.mu.Unlock()

	// 2) 本批索引:重复 id 先到先得,后来者记失败。
	type entry struct {
		p    contract.Plugin
		meta contract.Meta
	}
	entries := make([]entry, 0, len(plugs))
	byID := map[string]bool{}
	for _, p := range plugs {
		m := p.Meta()
		if m.ID == "" {
			rep.Failed = append(rep.Failed, RegisterFailure{ID: "", Err: fmt.Errorf("host: empty id rejected")})
			continue
		}
		if byID[m.ID] {
			rep.Failed = append(rep.Failed, RegisterFailure{ID: m.ID, Err: fmt.Errorf("host: plugin %s: duplicate id in batch", m.ID)})
			continue
		}
		byID[m.ID] = true
		entries = append(entries, entry{p: p, meta: m})
	}

	// 3) 提供者表:key → 提供该 key 的本批 entry 下标。
	provide := map[string][]int{}
	for i, e := range entries {
		for _, k := range provideKeys(e.meta) {
			provide[k] = append(provide[k], i)
		}
	}

	// 4) 固定点:剔除"存在不可达依赖"的条目(缺依赖;一个被剔的条目可能让它
	// 的依赖者也没了供给,故需反复收紧)。依赖键若在 eventually(待补载/被转为待补载)
	// 里,则本条目**转为待补载**而非失败。
	n := len(entries)
	active := make([]bool, n)
	for i := range active {
		active[i] = true
	}
	deferred := map[int]bool{}
	for {
		removed := false
		for i := range entries {
			if !active[i] {
				continue
			}
			for _, k := range needKeys(entries[i].meta) {
				if h.keyProvided(k, registered, registeredSlot) || hasActiveProvider(provide[k], active) {
					continue
				}
				active[i] = false
				if eventually[k] {
					// 依赖要等 pending/被转为待补载的条目:本条目也转为待补载。
					deferred[i] = true
					for _, pk := range provideKeys(entries[i].meta) {
						eventually[pk] = true
					}
				} else {
					rep.Failed = append(rep.Failed, RegisterFailure{ID: entries[i].meta.ID, Err: fmt.Errorf("host: plugin %s: missing dependency %s", entries[i].meta.ID, prettyKey(k))})
				}
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}

	// 5) Kahn 拓扑排序(活跃条目;同一依赖键只计一次入度,slot 多实现者以任一个
	// 供给即可打开该键)。
	indegree := make([]int, n)
	waiters := map[string][]int{}
	for i := range entries {
		if !active[i] {
			continue
		}
		seen := map[string]bool{}
		for _, k := range needKeys(entries[i].meta) {
			if h.keyProvided(k, registered, registeredSlot) || seen[k] {
				continue
			}
			seen[k] = true
			indegree[i]++
			waiters[k] = append(waiters[k], i)
		}
	}
	queue := make([]int, 0, n)
	for i := range entries {
		if active[i] && indegree[i] == 0 {
			queue = append(queue, i)
		}
	}
	var order []int
	opened := map[string]bool{}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		order = append(order, i)
		for _, k := range provideKeys(entries[i].meta) {
			if opened[k] {
				continue
			}
			opened[k] = true
			for _, w := range waiters[k] {
				indegree[w]--
				if indegree[w] == 0 {
					queue = append(queue, w)
				}
			}
		}
	}

	// 6) 未入序的活跃条目 = 成环。
	emitted := map[int]bool{}
	for _, i := range order {
		emitted[i] = true
	}
	for i := range entries {
		if active[i] && !emitted[i] {
			rep.Failed = append(rep.Failed, RegisterFailure{ID: entries[i].meta.ID, Err: fmt.Errorf("host: plugin %s: dependency cycle", entries[i].meta.ID)})
		}
	}

	// 7) 依序注册(复用单件 Register 的完整校验;失败条目继续隔离)。
	for _, i := range order {
		if err := h.Register(entries[i].p); err != nil {
			rep.Failed = append(rep.Failed, RegisterFailure{ID: entries[i].meta.ID, Err: err})
			continue
		}
		rep.Registered = append(rep.Registered, entries[i].meta.ID)
	}

	// 8) 依赖 pending 插件(或本批被转为待补载)的条目 → 转入待补载队列,随依赖到位自动补载。
	for i := range entries {
		if deferred[i] {
			if _, err := h.RegisterLazy(entries[i].p); err != nil {
				rep.Failed = append(rep.Failed, RegisterFailure{ID: entries[i].meta.ID, Err: err})
				continue
			}
			rep.Pending = append(rep.Pending, entries[i].meta.ID)
		}
	}
	return rep
}
