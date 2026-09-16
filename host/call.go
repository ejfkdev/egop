// 调用与配置链路:函数调用(含 schema 校验)、配置下发/合并/读回、工具面收集
// 与 hook 触发(observe 点按声明丢弃 Block)。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/schema"
)

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

// SetConfig 下发配置（配置 Schema 校验 + Configurable）。插件 ApplyConfig panic
// 归一到 error(机制层 fail-closed)。配置写链路全程经 cfgMu 串行(并发下发不交错)。
func (h *Host[C]) SetConfig(pluginID string, cfg json.RawMessage) (err error) {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	return h.setConfig(pluginID, cfg)
}

// setConfig 是 SetConfig 的本体(要求已持 cfgMu;SetConfigField 合并后复用)。
func (h *Host[C]) setConfig(pluginID string, cfg json.RawMessage) (err error) {
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
	// 配置生效观察事件(框架级;payload={plugin,config})。config 投影做敏感键
	// 脱敏(host/redact.go)——观察面不落密钥;applied 全值保留供热更回灌。
	// 脱敏**声明优先**:顶层命中 ConfigFieldSpec.Secret 的键强制遮(键名不敏感也遮),
	// 键名子串启发(token/secret/…)作兜底——声明真源优先于启发。
	if h.opts.Events != nil {
		secretKeys := map[string]bool{}
		for _, f := range m.Provides.Config {
			if f.Secret {
				secretKeys[f.Key] = true
			}
		}
		payload, _ := json.Marshal(map[string]any{"plugin": pluginID, "config": redactJSONWith(cfg, secretKeys)})
		h.opts.Events.Dispatch(context.Background(), contract.Event{
			Type:    contract.EventConfigUpdated,
			Version: contract.EnvelopeVersion,
			Source:  &contract.Origin{Kind: contract.OriginHost, At: time.Now().UnixMilli()},
			Payload: payload,
		})
	}
	return nil
}

// SetConfigField 合并单字段进整份生效配置(读旧 applied→补 key→整对象下发)。egop 层
// 无能力门控(UI/装配层单字段保存用);与 SetConfig(整对象替换)区别开。
// 读改写在 cfgMu 内一次完成:并发写不同字段互不丢更新。
func (h *Host[C]) SetConfigField(pluginID, key string, value json.RawMessage) error {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.mu.Lock()
	raw := h.applied[pluginID]
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
	return h.setConfig(pluginID, full)
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

// Tools 包装声明 CapTools 的 ToolProvider[C] 为 Tool 适配。
type Tool[C any] struct {
	Spec     contract.FuncSpec
	run      contract.ToolFunc[C]
	pluginID string
	host     *Host[C]
}

func (t *Tool[C]) Info() contract.FuncSpec { return t.Spec }

// Tools 收集全部插件工具。**锁内只取 provider 快照,锁外调插件代码**——
// ToolSpecs/Tool 是插件实现(wasm 形态会进 guest),持 h.mu 调用会与宿主注入
// `plugins`/`call`(它们都取 h.mu)构成同 goroutine 死锁,且看门狗无解;
// 快照后插件即便被并发卸载也只影响本次 best-effort 收集(失败/空面跳过)。
func (h *Host[C]) Tools() []Tool[C] {
	type row struct {
		id string
		tp contract.ToolProvider[C]
	}
	h.mu.Lock()
	rows := make([]row, 0, len(h.plugins))
	for id, p := range h.plugins {
		tp, ok := p.(contract.ToolProvider[C])
		if !ok || !contract.HasCapability(h.meta[id], contract.CapTools) {
			continue
		}
		rows = append(rows, row{id: id, tp: tp})
	}
	h.mu.Unlock()
	var out []Tool[C]
	for _, r := range rows {
		// 工具面收集 best-effort:坏工具声明/生成 panic 跳过该插件,不 crash 宿主。
		func() {
			defer func() { _ = recover() }()
			for _, spec := range r.tp.ToolSpecs() {
				if fn, ok := r.tp.Tool(spec.Name); ok {
					out = append(out, Tool[C]{Spec: spec, run: fn, pluginID: r.id, host: h})
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
// 声明为 observe 的点(声明者优先):回调的 Block 无效——收集结果时丢弃并
// 注明 Reason(观察者不得阻断流程;声明面是 HookPointSpec.Kind)。
func (h *Host[C]) TriggerHook(ctx context.Context, hookID string, data json.RawMessage) []contract.HookResult {
	if h.opts.Hooks == nil {
		return nil
	}
	// 无调用来源时注入框架来源(与 Call 一致):hook 回调经 OriginFrom 知道"哪个点触发"。
	if contract.OriginFrom(ctx) == nil {
		ctx = contract.WithOrigin(ctx, &contract.Origin{Kind: contract.OriginHost, Point: hookID, At: time.Now().UnixMilli()})
	}
	results := h.opts.Hooks.Trigger(ctx, hookID, data)
	h.mu.Lock()
	kind := contract.KindModify
	if d, ok := h.hookDecls[hookID]; ok {
		kind = d.kind
	}
	h.mu.Unlock()
	if kind != contract.KindObserve {
		return results
	}
	for i := range results {
		if results[i].Block {
			results[i].Block = false
			if results[i].Reason != "" {
				results[i].Reason += "; "
			}
			results[i].Reason += "block ignored (observe-only hook point)"
			h.logf("host: hook %q: block from callback dropped (observe-only point)", hookID)
		}
	}
	return results
}
