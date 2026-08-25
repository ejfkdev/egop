// 开箱默认装配件的直接语义测试:内存事件总线、hook 总线、settings、点位集。
// host_test 已透过宿主间接覆盖,这里把它们的独立契约(幂等撤销/多订阅者/
// 框架回填/注册序)逐一钉死。
package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestMemEventsSubscribeDispatchUnsubscribe(t *testing.T) {
	m := NewMemEvents()
	var got []contract.Event
	un := m.Subscribe(&contract.EventFilter{Type: "t"}, func(_ context.Context, e contract.Event) { got = append(got, e) })
	m.Dispatch(context.Background(), contract.Event{Type: "t", Payload: json.RawMessage(`{"n":1}`), Source: &contract.Origin{ID: "src", Kind: contract.OriginEvent}})
	if len(got) != 1 || string(got[0].Payload) != `{"n":1}` || got[0].Source == nil || got[0].Source.ID != "src" {
		t.Fatalf("dispatch got = %+v", got)
	}
	un() // 幂等撤销
	un()
	m.Dispatch(context.Background(), contract.Event{Type: "t", Payload: json.RawMessage(`{}`)})
	if len(got) != 1 {
		t.Fatalf("unsubscribe should stop delivery, got %d", len(got))
	}
}

func TestMemEventsMultipleSubscribersIndependent(t *testing.T) {
	m := NewMemEvents()
	var a, b []string
	m.Subscribe(&contract.EventFilter{Type: "t"}, func(_ context.Context, e contract.Event) { a = append(a, "a") })
	unB := m.Subscribe(&contract.EventFilter{Type: "t"}, func(_ context.Context, e contract.Event) { b = append(b, "b") })
	m.Dispatch(context.Background(), contract.Event{Type: "t", Payload: json.RawMessage(`1`)})
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("both subscribers should get event: a=%v b=%v", a, b)
	}
	unB() // 撤销 b 只移除自己,a 仍收到
	m.Dispatch(context.Background(), contract.Event{Type: "t", Payload: json.RawMessage(`1`)})
	if len(a) != 2 || len(b) != 1 {
		t.Fatalf("unsubscribe b only removes b: a=%v b=%v", a, b)
	}
}

func TestMemEventsTopics(t *testing.T) {
	m := NewMemEvents()
	m.EnsureTopic("x")
	m.EnsureTopic("y")
	m.EnsureTopic("y") // 幂等
	if got := m.Topics(); len(got) != 2 {
		t.Fatalf("Topics = %v", got)
	}
}

func TestMemEventsNilFilterMatchesAll(t *testing.T) {
	m := NewMemEvents()
	var got []string
	m.Subscribe(nil, func(_ context.Context, e contract.Event) { got = append(got, e.Type) })
	m.Dispatch(context.Background(), contract.Event{Type: "a", Payload: json.RawMessage(`1`)})
	m.Dispatch(context.Background(), contract.Event{Type: "b", Payload: json.RawMessage(`1`)})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("nil filter should match all events: %v", got)
	}
}

func TestMemHooksTriggerFillsContextAndSorts(t *testing.T) {
	m := NewMemHooks()
	var order []int
	m.On("p1", "hook.x", func(_ context.Context, _ string, _ json.RawMessage) any { order = append(order, 1); return nil })
	m.On("p2", "hook.x", func(_ context.Context, _ string, _ json.RawMessage) any {
		order = append(order, 2)
		return contract.HookResult{Block: true}
	})
	m.On("p3", "hook.x", func(_ context.Context, _ string, _ json.RawMessage) any { order = append(order, 3); return "raw" })

	rs := m.Trigger(context.Background(), "hook.x", json.RawMessage(`{}`))
	if len(rs) != 3 {
		t.Fatalf("Trigger results = %d", len(rs))
	}
	// 注册序:先注册先触发(框架按 id 升序,id 即注册序)。
	if order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("trigger order = %v", order)
	}
	// 框架回填 :Origin 与 Seq;回调写的 Block / 归一 Data 保留。
	for i, r := range rs {
		if r.Seq != i+1 {
			t.Fatalf("result[%d].Seq = %d", i, r.Seq)
		}
		if r.Origin == nil || r.Origin.Kind != contract.OriginHook || r.Origin.Point != "hook.x" {
			t.Fatalf("result[%d].Origin = %+v", i, r.Origin)
		}
	}
	if owners := []string{rs[0].Origin.ID, rs[1].Origin.ID, rs[2].Origin.ID}; owners[0] != "p1" || owners[1] != "p2" || owners[2] != "p3" {
		t.Fatalf("origins should carry owners p1/p2/p3: %v", owners)
	}
	if !rs[1].Block {
		t.Fatal("callback-written Block should be preserved")
	}
	if string(rs[2].Data) != `"raw"` {
		t.Fatalf("raw return should be normalized to JSON data: %s", rs[2].Data)
	}
}

func TestMemHooksUnsubscribe(t *testing.T) {
	m := NewMemHooks()
	n := 0
	un := m.On("p", "h", func(_ context.Context, _ string, _ json.RawMessage) any { n++; return nil })
	un()
	un() // 幂等
	m.Trigger(context.Background(), "h", json.RawMessage(`{}`))
	if n != 0 {
		t.Fatalf("unsubscribed hook still fired %d times", n)
	}
}

func TestMapSettingsAndPoints(t *testing.T) {
	s := NewMapSettings()
	if _, ok := s.Get("missing"); ok {
		t.Fatal("missing key should be not-found")
	}
	s.Set("k", json.RawMessage(`1`))
	v, ok := s.Get("k")
	if !ok || string(v) != "1" {
		t.Fatalf("Get = %s, %v", v, ok)
	}
	if got := s.Keys(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("Keys = %v", got)
	}

	p := NewMemPoints()
	p.EnsurePoint("a")
	p.EnsurePoint("a") // 幂等
	p.EnsurePoint("b")
	if got := p.Points(); len(got) != 2 {
		t.Fatalf("Points = %v", got)
	}
}
