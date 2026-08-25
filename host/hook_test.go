package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestHookTriggerAndBlock(t *testing.T) {
	h := New[any](Options[any]{})
	var order []string
	unsub1 := h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
		order = append(order, "cb1")
		return json.RawMessage(`"ok1"`) // 直接返回数据,框架包成 HookResult{Data:"ok1"}
	})
	h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
		order = append(order, "cb2")
		return contract.HookResult{Block: true, Reason: "for test", Data: json.RawMessage(`{"score":42}`)}
	})

	results := h.TriggerHook(context.Background(), "demo.validate", json.RawMessage(`{"x":1}`))
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if len(order) != 2 || order[0] != "cb1" || order[1] != "cb2" {
		t.Fatalf("callbacks = %v", order)
	}
	if results[0].Block || !results[1].Block {
		t.Fatalf("block flags = %+v / %+v", results[0], results[1])
	}
	if results[1].Reason != "for test" || string(results[1].Data) != `{"score":42}` {
		t.Fatalf("reason/data = %q / %s", results[1].Reason, results[1].Data)
	}
	// 框架回填的执行上下文:顺序、时间戳、产生者(统一进 Origin)。
	if results[0].Seq != 1 || results[1].Seq != 2 {
		t.Fatalf("seq = %d / %d", results[0].Seq, results[1].Seq)
	}
	if results[0].Origin == nil || results[1].Origin == nil ||
		results[0].Origin.At == 0 || results[1].Origin.At == 0 {
		t.Fatalf("origin at not filled: %+v / %+v", results[0].Origin, results[1].Origin)
	}
	if results[1].Origin.Kind != contract.OriginHook {
		t.Fatalf("hook origin kind = %q, want hook", results[1].Origin.Kind)
	}

	// 撤销后不再触发。
	unsub1()
	results = h.TriggerHook(context.Background(), "demo.validate", json.RawMessage(`{}`))
	if len(results) != 1 {
		t.Fatalf("after unsub results = %d", len(results))
	}
}

type subPlugin struct {
	meta     contract.Meta
	surface  contract.Surface
	register bool // 是否在 SetSurface 里注册 hook(用于对照热替换回滚)
}

func (p *subPlugin) Meta() contract.Meta { return p.meta }

func (p *subPlugin) SetSurface(s contract.Surface) {
	p.surface = s
	if !p.register {
		return
	}
	// 插件在生命周期内注册 hook 回调,但不显式撤销——依赖宿主自动回滚。
	s.OnHook("demo.hook", func(ctx context.Context, hookID string, data json.RawMessage) any {
		return nil // 直接返回 nil:框架包成空 HookResult(不阻断)
	})
}

func TestEffectAutoRollbackOnRemove(t *testing.T) {
	// 插件注册的 hook/事件订阅,应在被卸载时自动回滚,无需插件自查。
	h := New[any](Options[any]{})
	p := &subPlugin{meta: contract.Meta{ID: "p.sub", Name: "S", Version: "1"}, register: true}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	if n := len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))); n != 1 {
		t.Fatalf("before remove triggers = %d", n)
	}
	if _, err := h.Remove("p.sub", true); err != nil {
		t.Fatal(err)
	}
	if n := len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))); n != 0 {
		t.Fatalf("after remove triggers = %d (want 0, effect not rolled back)", n)
	}
}

func TestHookOriginFields(t *testing.T) {
	// hook 的来源统一进 Origin:插件注册的回调,触发时回填 ID/Kind:"hook"/Point/At。
	h := New[any](Options[any]{})
	p := &subPlugin{meta: contract.Meta{ID: "p.owner", Name: "O", Version: "1"}, register: true}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	results := h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	o := results[0].Origin
	if o == nil || o.ID != "p.owner" || o.Kind != contract.OriginHook || o.Point != "demo.hook" || o.At == 0 {
		t.Fatalf("hook origin = %+v (want id=p.owner kind=hook point=demo.hook at!=0)", o)
	}
}

func TestEffectAutoRollbackOnReplace(t *testing.T) {
	// 热替换同 id 插件时,旧实现注册的 hook/订阅应随 effect 撤销栈一并回滚。
	h := New[any](Options[any]{})
	p1 := &subPlugin{meta: contract.Meta{ID: "p.sub", Name: "S", Version: "1"}, register: true}
	if err := h.Register(p1); err != nil {
		t.Fatal(err)
	}
	if n := len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))); n != 1 {
		t.Fatalf("before replace triggers = %d", n)
	}
	p2 := &subPlugin{meta: contract.Meta{ID: "p.sub", Name: "S", Version: "2"}, register: false}
	if err := h.Replace(p2); err != nil {
		t.Fatal(err)
	}
	if n := len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`))); n != 0 {
		t.Fatalf("after replace triggers = %d (want 0, old effect not rolled back)", n)
	}
}

func TestHookReturnsRawData(t *testing.T) {
	// 回调直接返回数据(string/map),框架归一成 HookResult(Block=false,Data=JSON)。
	h := New[any](Options[any]{})
	h.OnHook("demo.raw", func(ctx context.Context, hookID string, data json.RawMessage) any {
		return "plain-string"
	})
	h.OnHook("demo.raw", func(ctx context.Context, hookID string, data json.RawMessage) any {
		return map[string]any{"n": 7}
	})

	results := h.TriggerHook(context.Background(), "demo.raw", json.RawMessage(`{}`))
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].Block || string(results[0].Data) != `"plain-string"` {
		t.Fatalf("string wrap = %+v", results[0])
	}
	if results[1].Block || string(results[1].Data) != `{"n":7}` {
		t.Fatalf("map wrap = %+v", results[1])
	}
	if results[0].Seq != 1 || results[1].Seq != 2 {
		t.Fatalf("seq = %d/%d", results[0].Seq, results[1].Seq)
	}
}

func TestHookResultOf(t *testing.T) {
	// nil → 空结果
	r := contract.HookResultOf(nil)
	if r.Block || len(r.Data) != 0 || r.Reason != "" {
		t.Fatalf("nil = %+v", r)
	}
	// HookResult → 原样
	full := contract.HookResult{Block: true, Reason: "x", Data: json.RawMessage(`"y"`)}
	r = contract.HookResultOf(full)
	if !r.Block || r.Reason != "x" || string(r.Data) != `"y"` {
		t.Fatalf("HookResult = %+v", r)
	}
	// *HookResult → 原样
	r = contract.HookResultOf(&full)
	if !r.Block || r.Reason != "x" {
		t.Fatalf("*HookResult = %+v", r)
	}
	// json.RawMessage → 原样字节
	r = contract.HookResultOf(json.RawMessage(`{"a":1}`))
	if r.Block || string(r.Data) != `{"a":1}` {
		t.Fatalf("json.RawMessage = %+v", r)
	}
	// []byte → 视作 JSON 字节
	r = contract.HookResultOf([]byte(`[1,2]`))
	if string(r.Data) != `[1,2]` {
		t.Fatalf("[]byte = %+v", r)
	}
	// string → JSON 字符串
	r = contract.HookResultOf("hi")
	if string(r.Data) != `"hi"` {
		t.Fatalf("string = %+v", r)
	}
	// struct → JSON 对象
	r = contract.HookResultOf(struct {
		X int `json:"x"`
	}{X: 3})
	if string(r.Data) != `{"x":3}` {
		t.Fatalf("struct = %+v", r)
	}
	// 不可序列化 → Reason 记错误(仍非阻断)
	r = contract.HookResultOf(make(chan int))
	if r.Reason == "" || r.Block {
		t.Fatalf("unmarshalable = %+v", r)
	}
}
