// 槽位依赖卸载语义自检:Remove/Dependents 对"点槽位"依赖的 fail-closed 判定、
// 级联 victims 去重(removed 事件不重复广播)、Replace 与 Register 同款契约校验。
package host

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// slotHost 构造带 "enc" 槽位目录的宿主(A 占据;B 点槽位依赖)。
func slotHost(t *testing.T, ev Events) *Host[any] {
	t.Helper()
	slots := map[string]contract.SlotSpec{"enc": {ID: "enc", Doc: "encoder"}}
	h := New[any](Options[any]{Events: ev, SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	a := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "1", Slot: "enc"}}
	b := &demoPlugin{meta: contract.Meta{ID: "B", Name: "B", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Slot: "enc", Kind: contract.DepInit}}}}}
	if err := h.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(b); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestRemoveSlotDependentFailClosed 槽位唯一供给者被点名依赖:非级联卸载必须拒绝
// (旧实现拿槽位名与插件 id 比较,判不出这条边 → B 悬空)。
func TestRemoveSlotDependentFailClosed(t *testing.T) {
	h := slotHost(t, nil)
	if _, err := h.Remove("A", false); err == nil {
		t.Fatal("removing sole slot provider must be refused while B slot-depends on it")
	}
	if !h.HasPlugin("A") || !h.HasPlugin("B") {
		t.Fatal("refused remove must keep registry intact")
	}
	deps := h.Dependents("A")
	if len(deps) != 1 || deps[0] != "B" {
		t.Fatalf("Dependents(A) = %v, want [B]", deps)
	}
	// 级联:A+B 一起走。
	victims, err := h.Remove("A", true)
	if err != nil || len(victims) != 2 {
		t.Fatalf("cascade victims = %v, err = %v", victims, err)
	}
	if h.HasPlugin("A") || h.HasPlugin("B") {
		t.Fatal("cascade must unload both")
	}
}

// TestRemoveSlotDependentOtherOccupant 槽位仍有其它供给者时,删一个占据者不破坏
// 依赖——判定按"删除后槽位是否仍被满足",不做过度拒绝。
func TestRemoveSlotDependentOtherOccupant(t *testing.T) {
	slots := map[string]contract.SlotSpec{"enc": {ID: "enc", Doc: "encoder"}}
	h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	for _, p := range []*demoPlugin{
		{meta: contract.Meta{ID: "A1", Name: "A1", Version: "1", Slot: "enc"}},
		{meta: contract.Meta{ID: "A2", Name: "A2", Version: "1", Slot: "enc"}},
		{meta: contract.Meta{ID: "B", Name: "B", Version: "1",
			Requires: contract.Requires{Deps: []contract.Dependency{{Slot: "enc", Kind: contract.DepInit}}}}},
	} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if deps := h.Dependents("A1"); len(deps) != 0 {
		t.Fatalf("Dependents(A1) = %v, want empty (slot still satisfied by A2)", deps)
	}
	victims, err := h.Remove("A1", false)
	if err != nil || len(victims) != 1 || victims[0] != "A1" {
		t.Fatalf("remove A1 = %v, %v; want [A1] only", victims, err)
	}
	if !h.HasPlugin("B") || !h.HasPlugin("A2") {
		t.Fatal("B and A2 must survive")
	}
}

// TestRemoveCascadeDedup 一个依赖者多条边指向被删集合:victims 只计一次,
// plugin.removed 事件也只广播一次(软依赖订阅者的可观察契约)。
func TestRemoveCascadeDedup(t *testing.T) {
	ev := NewMemEvents()
	var mu sync.Mutex
	var removedB int
	ev.Subscribe(&contract.EventFilter{Type: contract.EventPluginRemoved}, func(_ context.Context, e contract.Event) {
		var payload struct {
			Plugin string `json:"plugin"`
		}
		_ = json.Unmarshal(e.Payload, &payload)
		if payload.Plugin == "B" {
			mu.Lock()
			removedB++
			mu.Unlock()
		}
	})
	h := New[any](Options[any]{Events: ev})
	a := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "1"}}
	b := &demoPlugin{meta: contract.Meta{ID: "B", Name: "B", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{
			{Plugin: "A", Kind: contract.DepInit},
			{Plugin: "A", Kind: contract.DepInit},
		}}}}
	if err := h.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(b); err != nil {
		t.Fatal(err)
	}
	victims, err := h.Remove("A", true)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, v := range victims {
		count[v]++
	}
	if count["B"] != 1 || count["A"] != 1 || len(victims) != 2 {
		t.Fatalf("victims = %v, want [A B] each once", victims)
	}
	mu.Lock()
	defer mu.Unlock()
	if removedB != 1 {
		t.Fatalf("plugin.removed for B broadcast %d times, want exactly 1", removedB)
	}
}

// TestReplaceValidatesContract 热替换与首注册同款契约校验:槽位最小契约不满足、
// DepInit 依赖未就位 → 拒换且旧版原样在册(替换口不得比注册口宽松)。
func TestReplaceValidatesContract(t *testing.T) {
	slots := map[string]contract.SlotSpec{"enc": {ID: "enc", Doc: "encoder", Functions: []string{"ping"}}}
	h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	good := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "1", Slot: "enc",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}}}}
	if err := h.Register(good); err != nil {
		t.Fatal(err)
	}
	// 违背槽位契约(缺 ping)→ 拒换,旧版在册。
	bad := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "2", Slot: "enc"}}
	if err := h.Replace(bad); err == nil {
		t.Fatal("replace violating slot contract must be refused")
	}
	if h.meta["A"].Version != "1" {
		t.Fatalf("refused replace must keep old version, got %s", h.meta["A"].Version)
	}
	// 新增未满足的 DepInit 依赖 → 拒换。
	dep := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "2", Slot: "enc",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "ghost", Kind: contract.DepInit}}}}}
	if err := h.Replace(dep); err == nil {
		t.Fatal("replace with unmet init dep must be refused")
	}
	// 合规新版 → 换入。
	v2 := &demoPlugin{meta: contract.Meta{ID: "A", Name: "A", Version: "2", Slot: "enc",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}}}}
	if err := h.Replace(v2); err != nil {
		t.Fatalf("valid replace refused: %v", err)
	}
	if h.meta["A"].Version != "2" {
		t.Fatalf("replace did not take effect: %s", h.meta["A"].Version)
	}
}

// TestReplaceReEnsuresDeclarations 热替换的新声明面(点位/hook 点/事件主题)与
// 首注册一致地补落——替换不只是换实例,声明簿记同样刷新。
func TestReplaceReEnsuresDeclarations(t *testing.T) {
	pts := NewMemPoints()
	ev := NewMemEvents()
	h := New[any](Options[any]{Points: pts, Events: ev})
	v1 := &demoPlugin{meta: contract.Meta{ID: "R", Name: "R", Version: "1"}}
	if err := h.Register(v1); err != nil {
		t.Fatal(err)
	}
	v2 := &demoPlugin{meta: contract.Meta{ID: "R", Name: "R", Version: "2",
		Provides: contract.Provides{
			Points: []string{"run.new"},
			Hooks:  []contract.HookPointSpec{{ID: "h.new", Kind: contract.KindObserve}},
			Events: []contract.EventTopicSpec{{ID: "e.new"}},
		},
		Requires: contract.Requires{Listens: []string{"run.listen"}},
	}}
	if err := h.Replace(v2); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := map[string]bool{}
	for _, p := range pts.Points() {
		got[p] = true
	}
	for _, want := range []string{"run.new", "run.listen", contract.PointID("R", "h.new")} {
		if !got[want] {
			t.Fatalf("point %q not ensured on replace: %v", want, pts.Points())
		}
	}
	topics := map[string]bool{}
	for _, tp := range ev.Topics() {
		topics[tp] = true
	}
	if !topics[contract.EventID("R", "e.new")] {
		t.Fatalf("topic dyn.R.e.new not ensured on replace: %v", ev.Topics())
	}
}
