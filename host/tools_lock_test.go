package host

// Tools() 收集的锁纪律回归:锁内只取 provider 快照,锁外调 ToolSpecs/Tool
// (插件代码)。插件在收集回调里回查宿主(plugins/call 都取 h.mu)曾是
// 同 goroutine 死锁(wasm 活体工具面 egop_tool_specs 的真实形态)。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

type lockProbeTools struct {
	meta contract.Meta
	h    *Host[any]
	spec contract.FuncSpec
}

func (p *lockProbeTools) Meta() contract.Meta { return p.meta }

// ToolSpecs 回查宿主目录(旧代码在 h.mu 内调这里 → 同 goroutine 死锁)。
func (p *lockProbeTools) ToolSpecs() []contract.FuncSpec {
	_ = p.h.Plugins()
	return []contract.FuncSpec{p.spec}
}

// Tool 同样回查(经 Host.Call 取函数目录,也取 h.mu)。
func (p *lockProbeTools) Tool(name string) (contract.ToolFunc[any], bool) {
	if name != p.spec.Name {
		return nil, false
	}
	_, _ = p.h.Call(context.Background(), "lp.holder", "ping", json.RawMessage(`{}`))
	return func(ctx context.Context, _ *any, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"ok"`), nil
	}, true
}

type lpHolder struct{}

func (lpHolder) Meta() contract.Meta {
	return contract.Meta{ID: "lp.holder", Name: "L", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}}}
}
func (lpHolder) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"pong"`), nil
}

func TestToolsCallsPluginCodeOutsideLock(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(lpHolder{}); err != nil {
		t.Fatal(err)
	}
	probe := &lockProbeTools{
		meta: contract.Meta{
			ID: "lp.tools", Name: "T", Version: "1",
			Provides: contract.Provides{Capabilities: []string{contract.CapTools}},
		},
		h:    h,
		spec: contract.FuncSpec{Name: "probe"},
	}
	if err := h.Register(probe); err != nil {
		t.Fatal(err)
	}
	// 死锁=此处挂死(测试超时);正确行为:锁外调,收集成功。
	tools := h.Tools()
	if len(tools) != 1 || tools[0].Spec.Name != "probe" {
		t.Fatalf("tools = %+v", tools)
	}
	s, err := tools[0].Run(context.Background(), nil, json.RawMessage(`{}`))
	if err != nil || s != `"ok"` {
		t.Fatalf("tool run = %q, %v", s, err)
	}
}
