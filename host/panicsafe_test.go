// 插件 panic 隔离测试:函数调用 / 配置下发 / 工具 / hook / 事件订阅 五条插件代码
// 执行路径的 panic 都归一到 error(或 Reason),绝不炸穿宿主进程。
package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

type panicCallPlugin struct{ meta contract.Meta }

func (p *panicCallPlugin) Meta() contract.Meta { return p.meta }
func (p *panicCallPlugin) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	panic("boom-call")
}

func TestCallPanicIsolated(t *testing.T) {
	h := New[any](Options[any]{})
	_ = h.Register(&panicCallPlugin{meta: contract.Meta{ID: "p", Name: "P", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "fn"}}}}})
	_, err := h.Call(context.Background(), "p", "fn", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v (want panic isolated)", err)
	}
}

type panicCfgPlugin struct{ meta contract.Meta }

func (p *panicCfgPlugin) Meta() contract.Meta               { return p.meta }
func (p *panicCfgPlugin) ApplyConfig(json.RawMessage) error { panic("boom-cfg") }

func TestSetConfigPanicIsolated(t *testing.T) {
	h := New[any](Options[any]{})
	_ = h.Register(&panicCfgPlugin{meta: contract.Meta{ID: "c", Name: "C", Version: "1"}})
	if err := h.SetConfig("c", json.RawMessage(`{"x":1}`)); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v (want panic isolated)", err)
	}
}

type panicToolPlugin struct{ meta contract.Meta }

func (p *panicToolPlugin) Meta() contract.Meta { return p.meta }
func (p *panicToolPlugin) ToolSpecs() []contract.FuncSpec {
	return []contract.FuncSpec{{Name: "boom"}}
}
func (p *panicToolPlugin) Tool(_ string) (contract.ToolFunc[struct{}], bool) {
	return func(context.Context, *struct{}, json.RawMessage) (json.RawMessage, error) {
		panic("boom-tool")
	}, true
}

func TestToolRunPanicIsolated(t *testing.T) {
	h := New[struct{}](Options[struct{}]{})
	_ = h.Register(&panicToolPlugin{meta: contract.Meta{ID: "t", Name: "T", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapTools}}}})
	tools := h.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if _, err := tools[0].Run(context.Background(), &struct{}{}, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v (want panic isolated)", err)
	}
}

func TestHookPanicIsolated(t *testing.T) {
	h := New[any](Options[any]{})
	h.OnHook("demo.hook", func(context.Context, string, json.RawMessage) any { panic("boom-hook") })
	rs := h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))
	if len(rs) != 1 || !strings.Contains(rs[0].Reason, "panic") {
		t.Fatalf("hooks = %+v (want panic reason)", rs)
	}
}

func TestEventSubscriberPanicIsolated(t *testing.T) {
	m := NewMemEvents()
	second := 0
	m.Subscribe(&contract.EventFilter{Type: "t"}, func(context.Context, contract.Event) { panic("boom-event") })
	m.Subscribe(&contract.EventFilter{Type: "t"}, func(context.Context, contract.Event) { second++ })
	m.Dispatch(context.Background(), contract.Event{Type: "t", Payload: json.RawMessage(`{}`)})
	if second != 1 {
		t.Fatalf("second subscriber should still fire, got %d", second)
	}
}
