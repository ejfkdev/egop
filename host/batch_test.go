package host

import (
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// batchPlug 是只带 Meta 的最小插件(足够测试装载序,不参与函数/能力面)。
type batchPlug struct{ m contract.Meta }

func (p batchPlug) Meta() contract.Meta { return p.m }

func dep(plugin string) contract.Dependency {
	return contract.Dependency{Plugin: plugin, Kind: contract.DepInit}
}

func TestRegisterManyOrdersByDependency(t *testing.T) {
	h := New[any](Options[any]{})
	a := batchPlug{m: contract.Meta{ID: "a", Name: "A", Version: "1"}}
	b := batchPlug{m: contract.Meta{ID: "b", Name: "B", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("a")}}}}
	c := batchPlug{m: contract.Meta{ID: "c", Name: "C", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("b")}}}}

	// 乱序输入:依赖方在前、被依赖方在后。
	rep := h.RegisterMany([]contract.Plugin{c, b, a})
	if len(rep.Failed) != 0 {
		t.Fatalf("failed = %+v", rep.Failed)
	}
	want := []string{"a", "b", "c"}
	if len(rep.Registered) != len(want) {
		t.Fatalf("registered = %v", rep.Registered)
	}
	for i, id := range want {
		if rep.Registered[i] != id {
			t.Fatalf("order = %v, want %v", rep.Registered, want)
		}
	}
}

func TestRegisterManyMissingDependencyIsolated(t *testing.T) {
	h := New[any](Options[any]{})
	a := batchPlug{m: contract.Meta{ID: "a", Name: "A", Version: "1"}}
	ghost := batchPlug{m: contract.Meta{ID: "g", Name: "G", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("no.such")}}}}

	rep := h.RegisterMany([]contract.Plugin{ghost, a})
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0].Err.Error(), "missing dependency") {
		t.Fatalf("failed = %+v", rep.Failed)
	}
	if len(rep.Registered) != 1 || rep.Registered[0] != "a" {
		t.Fatalf("registered = %v", rep.Registered)
	}
}

func TestRegisterManyCycleDetected(t *testing.T) {
	h := New[any](Options[any]{})
	a := batchPlug{m: contract.Meta{ID: "a", Name: "A", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("b")}}}}
	b := batchPlug{m: contract.Meta{ID: "b", Name: "B", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("a")}}}}

	rep := h.RegisterMany([]contract.Plugin{a, b})
	if len(rep.Registered) != 0 {
		t.Fatalf("registered on cycle = %v", rep.Registered)
	}
	if len(rep.Failed) != 2 {
		t.Fatalf("failed = %+v", rep.Failed)
	}
	for _, f := range rep.Failed {
		if !strings.Contains(f.Err.Error(), "cycle") {
			t.Fatalf("unexpected error: %v", f.Err)
		}
	}
}

func TestRegisterManyDuplicateID(t *testing.T) {
	h := New[any](Options[any]{})
	a1 := batchPlug{m: contract.Meta{ID: "a", Name: "A1", Version: "1"}}
	a2 := batchPlug{m: contract.Meta{ID: "a", Name: "A2", Version: "2"}}

	rep := h.RegisterMany([]contract.Plugin{a1, a2})
	if len(rep.Registered) != 1 || rep.Registered[0] != "a" {
		t.Fatalf("registered = %v", rep.Registered)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0].Err.Error(), "duplicate") {
		t.Fatalf("failed = %+v", rep.Failed)
	}
}

func TestRegisterManyRespectsAlreadyRegistered(t *testing.T) {
	h := New[any](Options[any]{})
	a := batchPlug{m: contract.Meta{ID: "a", Name: "A", Version: "1"}}
	if err := h.Register(a); err != nil {
		t.Fatal(err)
	}
	b := batchPlug{m: contract.Meta{ID: "b", Name: "B", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{dep("a")}}}}

	rep := h.RegisterMany([]contract.Plugin{b})
	if len(rep.Failed) != 0 || len(rep.Registered) != 1 || rep.Registered[0] != "b" {
		t.Fatalf("rep = %+v", rep)
	}
}

func TestRegisterManySlotProvidedByBatch(t *testing.T) {
	slots := map[string]contract.SlotSpec{"demo.greeter": {ID: "demo.greeter", Doc: "d"}}
	h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	impl := batchPlug{m: contract.Meta{ID: "impl", Name: "I", Version: "1", Slot: "demo.greeter"}}
	consumer := batchPlug{m: contract.Meta{ID: "consumer", Name: "C", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{{Slot: "demo.greeter", Kind: contract.DepInit}}}}}

	// 乱序:消费者在前,槽位实现者在后。
	rep := h.RegisterMany([]contract.Plugin{consumer, impl})
	if len(rep.Failed) != 0 {
		t.Fatalf("failed = %+v", rep.Failed)
	}
	if len(rep.Registered) != 2 || rep.Registered[0] != "impl" || rep.Registered[1] != "consumer" {
		t.Fatalf("registered = %v", rep.Registered)
	}
}

func TestRegisterManyBuiltinSlotSatisfied(t *testing.T) {
	slots := map[string]contract.SlotSpec{"demo.builtin": {ID: "demo.builtin", Doc: "d", Builtin: true}}
	h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	p := batchPlug{m: contract.Meta{ID: "p", Name: "P", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{{Slot: "demo.builtin", Kind: contract.DepInit}}}}}

	rep := h.RegisterMany([]contract.Plugin{p})
	if len(rep.Failed) != 0 || len(rep.Registered) != 1 {
		t.Fatalf("rep = %+v", rep)
	}
}
