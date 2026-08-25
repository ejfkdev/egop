// 插件生命周期观察事件:Register/Remove/Replace 经总线广播
// plugin.registered / plugin.removed / plugin.replaced,软依赖(DepSoft)方订阅降级。
package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestLifecycleEvents(t *testing.T) {
	bus := NewMemEvents()
	h := New[any](Options[any]{Events: bus})

	type seen struct{ topic, plugin string }
	var got []seen
	for _, topic := range []string{
		contract.EventPluginRegistered,
		contract.EventPluginRemoved,
		contract.EventPluginReplaced,
	} {
		bus.Subscribe(&contract.EventFilter{Type: topic}, func(_ context.Context, e contract.Event) {
			var p struct {
				Plugin string `json:"plugin"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			got = append(got, seen{topic: e.Type, plugin: p.Plugin})
		})
	}

	_ = h.Register(&demoPlugin{meta: contract.Meta{ID: "p1", Name: "P1", Version: "1"}})
	_ = h.Register(&demoPlugin{meta: contract.Meta{ID: "p2", Name: "P2", Version: "1"}})
	_ = h.Replace(&demoPlugin{meta: contract.Meta{ID: "p1", Name: "P1", Version: "2"}})
	_, _ = h.Remove("p2", false)

	want := []seen{
		{contract.EventPluginRegistered, "p1"},
		{contract.EventPluginRegistered, "p2"},
		{contract.EventPluginReplaced, "p1"},
		{contract.EventPluginRemoved, "p2"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
