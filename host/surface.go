// plugSurface:按 Meta 能力声明裁剪的 Surface 视图(先说后做的单点强制——
// 未声明能力的方法返回 no-op/错误)。SurfaceFor 是远程/沙箱插件的能力回程入口。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/undo"
)

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
		// 丢弃必须留痕:guest 侧 publish 拿到 ok 无从感知(fire-and-forget 契约),
		// 静默丢曾让 wasm 插件整条事件流失效而宿主零观测(只余非插件源事件)。
		e.h.logf("host: plugin %s publish dropped (topic=%q): capability %q not declared or events bus not wired",
			e.meta.ID, ev.Type, contract.CapEmitsEvents)
		return
	}
	if ev.Type == "" {
		// 主题(topic)必填:无主题的事件无投递面,直接丢弃(fail-safe;同样留痕)。
		e.h.logf("host: plugin %s publish dropped: empty topic", e.meta.ID)
		return
	}
	if contract.IsFrameworkTopic(ev.Type) {
		// 框架保留主题(插件生命周期/配置观察)宿主专署:插件不得伪造
		// plugin.removed 等事件欺骗软依赖方/控制面(Source.Kind=host 是唯一真源)。
		e.h.logf("host: plugin %s publish dropped (topic=%q): framework-reserved topic", e.meta.ID, ev.Type)
		return
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
	raw := e.h.opts.Hooks.On(e.meta.ID, hookID, fn)
	// 同一撤销闭包可能被多处 Defer(进程内:宿主 effect 栈;wasm:主实例 unsubs
	// 与宿主 effect 栈各记一次)。用 Once 包一层,重复调用只反注册一次——
	// 换非幂等的 Hooks 后端也不致双注销(与 subscribe 同款)。
	var once sync.Once
	unsub := func() { once.Do(raw) }
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
func (e *plugSurface[C]) FS() (contract.FS, bool) {
	canRead, canWrite := e.caps[contract.CapFSRead], e.caps[contract.CapFSWrite]
	if (!canRead && !canWrite) || e.h.opts.FS == nil {
		return nil, false
	}
	// 单点强制:读写按声明分向门控(fsGuard);范围/沙箱策略在注入的实现里。
	return fsGuard{next: e.h.opts.FS, pluginID: e.meta.ID, canRead: canRead, canWrite: canWrite}, true
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
