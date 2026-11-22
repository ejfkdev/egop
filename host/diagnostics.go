package host

// Host.Diagnostics 是只读诊断快照：排障时“插件为什么还没起/现在什么态”
// 的宿主侧真源。内容=注册序快照(插件+版本+槽位+caps/deps 满足度+
// busy 探测+生效配置**脱敏**) + 函数目录 + 主题目录。全部只读、持锁快照、
// 不触碰插件代码(探测 Busy 是唯一越界调用,失败留 probe_err 不炸快照)。

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/ejfkdev/egop/contract"
)

// Diagnostics 是一次宿主诊断快照(只读;JSON 形为装配层 REST/CLI 直出)。
type Diagnostics struct {
	Plugins []DiagPlugin `json:"plugins"`
	Fns     []DiagFn     `json:"functions"`
	Topics  []string     `json:"topics"`
}

// DiagPlugin 单插件诊断条目。
type DiagPlugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Slot    string `json:"slot,omitempty"`
	// DepsUnmet 声明依赖未满足面(空=全满足)。装载门控原因可直接读:
	// 缺 init 依赖/槽位无实装/能力未接线,排障"为什么还没起"的第一现场。
	DepsUnmet []string `json:"deps_unmet,omitempty"`
	// Busy 在飞调用探测(Quiescer;nil=插件无探测面,"unknown")。
	Busy     *bool  `json:"busy,omitempty"`
	ProbeErr string `json:"probe_err,omitempty"`
	// Config 生效配置的**脱敏**投影(声明 Secret 键强制遮+键名启发兜底,
	// 与 config.updated 事件同口径;观察面永不落密钥)。
	Config json.RawMessage `json:"config,omitempty"`
}

// DiagFn 函数目录条目(插件归属+规格名)。
type DiagFn struct {
	Plugin string `json:"plugin"`
	Name   string `json:"name"`
}

// Diagnostics 采集快照(插件按 id 排序稳定输出;函数按 plugin.name 排序)。
func (h *Host[C]) Diagnostics() Diagnostics {
	h.mu.Lock()
	metas := make([]contract.Meta, 0, len(h.meta))
	for _, m := range h.meta {
		metas = append(metas, m)
	}
	fns := make([]DiagFn, 0, len(h.fns))
	for key, e := range h.fns {
		_ = key
		fns = append(fns, DiagFn{Plugin: e.pluginID, Name: e.spec.Name})
	}
	applied := map[string]json.RawMessage{}
	for id, cfg := range h.applied {
		applied[id] = cfg
	}
	h.mu.Unlock()

	out := Diagnostics{}
	for _, m := range metas {
		dp := DiagPlugin{ID: m.ID, Version: m.Version, Slot: m.Slot}
		// 依赖满足度:与装载门控同判定(uses h.mu 内部表,重新短持锁)。
		h.mu.Lock()
		dp.DepsUnmet = h.unmetDepsLocked(m)
		h.mu.Unlock()
		// busy 探测(只读 TryLock;无探测面=留空)。
		h.mu.Lock()
		p := h.plugins[m.ID]
		h.mu.Unlock()
		if q, ok := p.(Quiescer); ok && p != nil {
			busy, err := q.Busy()
			if err != nil {
				dp.ProbeErr = err.Error()
			} else {
				dp.Busy = &busy
			}
		}
		if cfg, ok := applied[m.ID]; ok && len(cfg) > 0 {
			secretKeys := map[string]bool{}
			for _, f := range m.Provides.Config {
				if f.Secret {
					secretKeys[f.Key] = true
				}
			}
			dp.Config = redactJSONWith(cfg, secretKeys)
		}
		out.Plugins = append(out.Plugins, dp)
	}
	sort.Slice(out.Plugins, func(i, j int) bool { return out.Plugins[i].ID < out.Plugins[j].ID })
	sort.Slice(fns, func(i, j int) bool {
		if fns[i].Plugin != fns[j].Plugin {
			return fns[i].Plugin < fns[j].Plugin
		}
		return fns[i].Name < fns[j].Name
	})
	out.Fns = fns
	if t, ok := h.opts.Events.(interface{ Topics() []string }); ok {
		out.Topics = t.Topics()
		sort.Strings(out.Topics)
	}
	return out
}

// unmetDepsLocked 声明依赖未满足清单(持 h.mu;与 validateContractLocked
// 同源判定,输出给人读:"init:agent.messenger"/"init:slot:react(版本)"/
// "slot-min:react"——在册插件的清单通常为空,此面主要服务装载期观察)。
func (h *Host[C]) unmetDepsLocked(m contract.Meta) []string {
	var unmet []string
	for _, r := range m.Requires.Deps {
		if r.Kind != contract.DepInit {
			continue
		}
		if r.Slot != "" {
			if !h.slotSatisfiedLocked(r.Slot) {
				unmet = append(unmet, "init:slot:"+r.Slot)
			}
			continue
		}
		if _, ok := h.plugins[r.Plugin]; !ok {
			unmet = append(unmet, "init:"+r.Plugin)
			continue
		}
		if r.MinVersion != "" && !versionAtLeast(h.meta[r.Plugin].Version, r.MinVersion) {
			unmet = append(unmet, "init:"+r.Plugin+"(版本 "+h.meta[r.Plugin].Version+" < "+r.MinVersion+")")
		}
	}
	if m.Slot != "" {
		if _, ok, _ := h.slotCheck(m); !ok {
			unmet = append(unmet, "slot:"+m.Slot+"(未知槽位)")
		} else if _, _, miss := h.slotCheck(m); len(miss) > 0 {
			unmet = append(unmet, "slot-min:"+m.Slot+"("+strings.Join(miss, "; ")+")")
		}
	}
	return unmet
}
