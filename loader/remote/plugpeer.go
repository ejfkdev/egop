// Go 插件作者侧助手:经 AttachStream/ServePluginStream 把既有的 remote.Stream 接进
// 会话并驱动。插件侧只需实现 PluginOps(六个回调),框架发来的调用/工具/hook/
// 配置/事件经会话引擎分发到这里。传输(连接建立)完全由外部注入,本包不建连接。
package remote

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// PluginOps 是插件作者侧的语义实现(全部可选——未实现即"不受理")。
type PluginOps struct {
	// CallFunc 处理框架→插件函数调用。
	CallFunc func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error)
	// Tool 处理框架→插件的工具调用(tctx 为线上 JSON,与 wasm ABI 同形)。
	Tool func(ctx context.Context, tool string, args, tctx json.RawMessage) (json.RawMessage, error)
	// Hook 处理框架→插件的 hook 回调触发(与进程内 HookFunc 同约定:可返回 HookResult
	// 或直接返回裸数据,框架经 HookResultOf 归一后传回)。
	Hook func(ctx context.Context, hookID string, data json.RawMessage) any
	// ApplyConfig 处理配置下发。
	ApplyConfig func(ctx context.Context, cfg json.RawMessage) error
	// PushEvent 接收命中订阅条件的事件推送(与进程内 SubscribeEventFilter 回调同签名
	// func(ctx, topic, e);完整 Event 含 Source/Labels;可选)。ctx 是本侧会话上下文,非
	// 发布者原始 ctx(ctx 不跨边界);投递事件为共享只读,订阅者不得改写。
	PushEvent func(ctx context.Context, topic string, e contract.Event)
}

// pluginPeer 是插件侧 peer 实现。
type pluginPeer struct {
	ops *PluginOps
}

func (pp *pluginPeer) HandleCall(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	if pp.ops.CallFunc == nil {
		return nil, fmt.Errorf("plugin function %q not implemented", fname)
	}
	return pp.ops.CallFunc(ctx, fname, input)
}

func (pp *pluginPeer) HandleTool(ctx context.Context, tool string, args, tctx json.RawMessage) (json.RawMessage, error) {
	if pp.ops.Tool == nil {
		return nil, fmt.Errorf("plugin tool %q not implemented", tool)
	}
	return pp.ops.Tool(ctx, tool, args, tctx)
}

func (pp *pluginPeer) HandleHook(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error) {
	if pp.ops.Hook == nil {
		return nil, fmt.Errorf("plugin hook %q not implemented", hookID)
	}
	// 与进程内 HookFunc 同约定:归一为 HookResult 后 JSON 传回(Block/Reason/Data)。
	return json.Marshal(contract.HookResultOf(pp.ops.Hook(ctx, hookID, data)))
}

func (pp *pluginPeer) HandleApplyConfig(ctx context.Context, cfg json.RawMessage) error {
	if pp.ops.ApplyConfig == nil {
		return fmt.Errorf("plugin not configurable")
	}
	return pp.ops.ApplyConfig(ctx, cfg)
}

func (pp *pluginPeer) HandleHostCall(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("remote: host_call not expected on plugin side")
}

func (pp *pluginPeer) HandleSubscribe(context.Context, *contract.EventFilter) {}
func (pp *pluginPeer) HandleShutdown(string)                                  {}
