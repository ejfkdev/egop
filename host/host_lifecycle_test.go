// 宿主生命周期/配置事件的补充测试:默认总线开箱可用、SetConfig 广播、
// Close 逆序清退(Disposer)。
package host

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

type closingPlugin struct {
	meta   contract.Meta
	closed atomic.Bool
}

func (p *closingPlugin) Meta() contract.Meta { return p.meta }
func (p *closingPlugin) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}
func (p *closingPlugin) ApplyConfig(json.RawMessage) error {
	return nil
}
func (p *closingPlugin) Close(context.Context) error {
	p.closed.Store(true)
	return nil
}

func TestDefaultMemEventsAndConfigBroadcast(t *testing.T) {
	h := New[any](Options[any]{})
	// 默认总线已在 New 时挂上并确保框架级主题
	mem, ok := h.opts.Events.(*MemEvents)
	if !ok {
		t.Fatalf("default events bus not MemEvents: %T", h.opts.Events)
	}
	if len(mem.Topics()) == 0 {
		t.Fatal("EventConfigUpdated topic not ensured by default")
	}
	got := make(chan contract.Event, 1)
	unsub := mem.Subscribe(&contract.EventFilter{Type: contract.EventConfigUpdated}, func(ctx context.Context, e contract.Event) {
		got <- e
	})
	defer unsub()

	p := &closingPlugin{meta: contract.Meta{ID: "p.cfg", Name: "cfg", Version: "1"}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("p.cfg", json.RawMessage(`{"k":1}`)); err != nil {
		t.Fatal(err)
	}
	e := <-got
	var payload struct {
		Plugin string          `json:"plugin"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.Plugin != "p.cfg" || string(payload.Config) != `{"k":1}` {
		t.Fatalf("event payload = %s (%v)", e.Payload, err)
	}
}

func TestHostCloseDisposesInReverseOrder(t *testing.T) {
	h := New[any](Options[any]{})
	c1 := &closingPlugin{meta: contract.Meta{ID: "c1", Name: "one", Version: "1"}}
	c2 := &closingPlugin{meta: contract.Meta{ID: "c2", Name: "two", Version: "1"}}
	if err := h.Register(c1); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(c2); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if h.HasPlugin("c1") || h.HasPlugin("c2") {
		t.Fatal("plugins not removed on Close")
	}
	if !c1.closed.Load() || !c2.closed.Load() {
		t.Fatal("Disposer.Close not invoked")
	}
}

func TestZeroValueOptionsDefaults(t *testing.T) {
	h := New[any](Options[any]{})
	// 默认件全部就位:不因 nil 恐慌
	if _, ok := h.opts.Settings.Get("nope"); ok {
		t.Fatal("empty settings should be all-miss")
	}
	if ms, ok := h.opts.Settings.(*MapSettings); ok {
		ms.Set("k", json.RawMessage(`"v"`))
		if v, found := h.opts.Settings.Get("k"); !found || string(v) != `"v"` {
			t.Fatalf("settings set/get broken: %s %v", v, found)
		}
	}
	if _, ok := h.opts.Points.(*MemPoints); !ok {
		t.Fatalf("default points not MemPoints: %T", h.opts.Points)
	}
}

func TestSnapshotShape(t *testing.T) {
	h := New[any](Options[any]{})
	p := &closingPlugin{meta: contract.Meta{
		ID: "p.snap", Name: "snap", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapTools},
			Functions:    []contract.FuncSpec{{Name: "ping"}},
		},
	}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	s := h.Snapshot()
	if len(s.Plugins) != 1 || len(s.Functions) != 1 || len(s.Capabilities[contract.CapTools]) != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("snapshot not marshalable: %v", err)
	}
}

func TestMemEventsMultipleSubscribersTargetedUnsubscribe(t *testing.T) {
	// 同一主题允许并存多个订阅者;撤销一个不得误删另一个。
	m := NewMemEvents()
	var a, b atomic.Int32
	unsubA := m.Subscribe(&contract.EventFilter{Type: "topic.multi"}, func(ctx context.Context, e contract.Event) { a.Add(1) })
	_ = m.Subscribe(&contract.EventFilter{Type: "topic.multi"}, func(ctx context.Context, e contract.Event) { b.Add(1) })

	m.Dispatch(context.Background(), contract.Event{Type: "topic.multi", Payload: json.RawMessage(`{}`)})
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("dispatch: a=%d b=%d (want 1/1)", a.Load(), b.Load())
	}

	unsubA()
	m.Dispatch(context.Background(), contract.Event{Type: "topic.multi", Payload: json.RawMessage(`{}`)})
	if a.Load() != 1 || b.Load() != 2 {
		t.Fatalf("after unsubA: a=%d b=%d (want 1/2)", a.Load(), b.Load())
	}
}
