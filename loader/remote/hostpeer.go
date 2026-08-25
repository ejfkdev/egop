// hostPeer 是框架侧的 peer 实现:插件经 HostCall 回程调用 Surface 面——一律先
// RemoteHost.SurfaceFor(pluginID) 取能力门控视图,与进程内插件同一语义(未声明即拒绝)。
// 业务能力经 Surface.Op 透传,守卫词由装配注入(host.Options)。
package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/loader"
	"github.com/ejfkdev/egop/undo"
)

// RemoteHost 是 remote 加载器的宿主面(loader.HostFace 的别名):
// core host.Host[C] 与装配层桥宿主都满足。
type RemoteHost = loader.HostFace

// hostPeer 是框架侧的连接对端语义。
type hostPeer struct {
	host     RemoteHost
	pluginID string
	mu       sync.Mutex
	pushFn   func(e contract.Event) // 订阅事件推帧口(会话构造后回填)
	hookFn   func(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error)
	unsubs   undo.Catcher
}

func newHostPeer(rh RemoteHost, pluginID string) *hostPeer {
	return &hostPeer{host: rh, pluginID: pluginID}
}

// SetPush 回填事件推帧口(会话与 peer 互相引用的接缝)。
func (hp *hostPeer) SetPush(fn func(e contract.Event)) {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	hp.pushFn = fn
}

// SetHook 回填 hook 触发帧口(框架→插件 HookCall)。
func (hp *hostPeer) SetHook(fn func(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error)) {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	hp.hookFn = fn
}

// HandleCall / HandleTool / HandleApplyConfig 是插件侧入口,框架侧不该到达。
func (hp *hostPeer) HandleCall(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: call not expected on host side")
}

func (hp *hostPeer) HandleTool(context.Context, string, json.RawMessage, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: tool not expected on host side")
}

func (hp *hostPeer) HandleHook(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: hook not expected on host side")
}

func (hp *hostPeer) HandleApplyConfig(context.Context, json.RawMessage) error {
	return errors.New("remote: apply_config not expected on host side")
}

// HandleShutdown 对端已断,会话层负责收起;此处无补充动作。
func (hp *hostPeer) HandleShutdown(string) {}

// HandleHostCall 路由 Surface 回程(op 词汇见下方;结果信封由会话层包装)。
func (hp *hostPeer) HandleHostCall(ctx context.Context, op string, input json.RawMessage) (json.RawMessage, error) {
	sur, ok := hp.host.SurfaceFor(hp.pluginID)
	if !ok {
		return nil, fmt.Errorf("plugin %s: not registered", hp.pluginID)
	}
	switch op {
	case OpCall:
		var a callArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		return sur.Call(ctx, a.PluginID, a.Fname, a.Input)
	case OpGetSetting:
		var a keyArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		v, found := sur.GetSetting(a.Key)
		return mustJSON(map[string]any{"found": found, "value": v}), nil
	case OpPersistRead:
		var a readArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		fs, ok := sur.Persist()
		if !ok {
			return nil, notAvailable(contract.CapPersist)
		}
		data, err := fs.Read(a.Name)
		if err != nil {
			return nil, err
		}
		return mustJSON(map[string]any{"data_b64": base64.StdEncoding.EncodeToString(data)}), nil
	case OpPersistWrite:
		var a writeArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		fs, ok := sur.Persist()
		if !ok {
			return nil, notAvailable(contract.CapPersist)
		}
		data, err := b64bytes(a.DataB64)
		if err != nil {
			return nil, fmt.Errorf("bad %s data_b64: %w", op, err)
		}
		return json.RawMessage("null"), fs.Write(a.Name, data)
	case OpPersistList:
		fs, ok := sur.Persist()
		if !ok {
			return nil, notAvailable(contract.CapPersist)
		}
		names, err := fs.List()
		if err != nil {
			return nil, err
		}
		return mustJSON(names), nil
	case OpKVGet:
		var a keyArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		kv, ok := sur.KV()
		if !ok {
			return nil, notAvailable(contract.CapKV)
		}
		v, found := kv.Get(a.Key)
		if !found {
			return mustJSON(map[string]any{"found": false}), nil
		}
		return mustJSON(map[string]any{"found": true, "value_b64": base64.StdEncoding.EncodeToString(v)}), nil
	case OpKVPut:
		var a putArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		kv, ok := sur.KV()
		if !ok {
			return nil, notAvailable(contract.CapKV)
		}
		v, err := b64bytes(a.ValueB64)
		if err != nil {
			return nil, fmt.Errorf("bad %s value_b64: %w", op, err)
		}
		kv.Put(a.Key, v)
		return json.RawMessage("null"), nil
	case OpKVDelete:
		var a keyArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		kv, ok := sur.KV()
		if !ok {
			return nil, notAvailable(contract.CapKV)
		}
		kv.Delete(a.Key)
		return json.RawMessage("null"), nil
	case OpKVKeys:
		kv, ok := sur.KV()
		if !ok {
			return nil, notAvailable(contract.CapKV)
		}
		return mustJSON(kv.Keys()), nil
	case OpExec:
		var a execArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		out, err := sur.Exec(ctx, a.Cmd)
		if err != nil {
			return nil, err
		}
		return mustJSON(map[string]any{"output": out}), nil
	case OpOnHook:
		var a struct {
			HookID string `json:"hook_id"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		sur.OnHook(a.HookID, func(ctx context.Context, hookID string, data json.RawMessage) any {
			hp.mu.Lock()
			fn := hp.hookFn
			hp.mu.Unlock()
			if fn == nil {
				return contract.HookResult{Reason: "remote: hook not wired"}
			}
			raw, err := fn(ctx, hookID, data)
			if err != nil {
				return contract.HookResult{Reason: err.Error()}
			}
			var hr contract.HookResult
			if err := json.Unmarshal(raw, &hr); err != nil {
				return contract.HookResult{Reason: err.Error()}
			}
			return hr
		})
		return json.RawMessage("null"), nil
	case OpPublishEvent:
		var a publishArgs
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		sur.Publish(ctx, contract.Event{
			Type:    a.Topic,
			SubType: a.SubType,
			Labels:  a.Labels,
			Payload: a.Payload,
		})
		return json.RawMessage("null"), nil
	case OpPlugins:
		return mustJSON(sur.Plugins()), nil
	case OpGetPlugin:
		var a struct {
			PluginID string `json:"plugin_id"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		m, found := sur.GetPlugin(a.PluginID)
		return mustJSON(map[string]any{"found": found, "meta": m}), nil
	case OpGetConfig:
		var a struct {
			PluginID string `json:"plugin_id"`
			Key      string `json:"key"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		v, found := sur.GetConfig(a.PluginID, a.Key)
		return mustJSON(map[string]any{"found": found, "value": v}), nil
	case OpSetConfig:
		var a struct {
			PluginID string          `json:"plugin_id"`
			Key      string          `json:"key"`
			Value    json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, fmt.Errorf("bad %s args: %w", op, err)
		}
		if err := sur.SetConfig(a.PluginID, a.Key, a.Value); err != nil {
			return nil, err
		}
		return json.RawMessage("null"), nil
	default:
		// 其余 op 一律经 Surface.Op 扩展:守卫词与处理器由装配层注入(Options.OpAliases/Ops)。
		return sur.Op(ctx, op, input)
	}
}

// HandleSubscribe 装事件订阅:框架广播命中过滤条件时把完整事件推回插件会话。
func (hp *hostPeer) HandleSubscribe(_ context.Context, f *contract.EventFilter) {
	sur, ok := hp.host.SurfaceFor(hp.pluginID)
	if !ok {
		return
	}
	unsub := sur.SubscribeEventFilter(f, func(_ context.Context, _ string, e contract.Event) {
		hp.mu.Lock()
		fn := hp.pushFn
		hp.mu.Unlock()
		if fn != nil {
			fn(e)
		}
	})
	hp.unsubs.Defer(unsub)
}

// UnsubAll 断连清理订阅(统一 effect 栈,反序清退)。
func (hp *hostPeer) UnsubAll() {
	_ = hp.unsubs.Close()
}

func notAvailable(cap string) error {
	return fmt.Errorf("capability %q not available", cap)
}

// mustJSON 是宿主侧非信封结果序列化助手。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
