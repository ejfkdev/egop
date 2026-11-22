package host

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
)

type mutableMetaPlugin struct {
	meta contract.Meta
}

func (p *mutableMetaPlugin) Meta() contract.Meta { return p.meta }
func (p *mutableMetaPlugin) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}
func (p *mutableMetaPlugin) ApplyConfig(json.RawMessage) error { return nil }

func TestRegisterFailureKeepsPendingAndCommitsFunctionsAtomically(t *testing.T) {
	t.Run("pending survives failed validation", func(t *testing.T) {
		h := New[any](Options[any]{})
		p := &mutableMetaPlugin{meta: contract.Meta{
			ID: "pending.invalid", Name: "P", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ok"}, {Name: "bad.name"}}},
			Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "base", Kind: contract.DepInit}}},
		}}
		if status, err := h.RegisterLazy(p); err != nil || status != StatusPending {
			t.Fatalf("RegisterLazy = %v, %v", status, err)
		}
		if err := h.Register(&mutableMetaPlugin{meta: contract.Meta{ID: "base", Name: "B", Version: "1"}}); err != nil {
			t.Fatal(err)
		}
		if h.HasPlugin(p.meta.ID) {
			t.Fatal("invalid pending plugin was registered")
		}

		// Fix the implementation. A later, unrelated registration must retry the
		// retained pending entry and register it without an explicit Register(p).
		p.meta.Provides.Functions = []contract.FuncSpec{{Name: "ok"}}
		if err := h.Register(&mutableMetaPlugin{meta: contract.Meta{ID: "trigger", Name: "T", Version: "1"}}); err != nil {
			t.Fatal(err)
		}
		if !h.HasPlugin(p.meta.ID) {
			t.Fatal("failed registration discarded the pending plugin")
		}
	})

	t.Run("function conflict leaves no earlier entries", func(t *testing.T) {
		h := New[any](Options[any]{})
		// Seed a pre-existing directory conflict (for example, one left by an old
		// process or a manually restored catalog), then attempt an atomic register.
		h.mu.Lock()
		h.fns["atomic.second"] = fnEntry{pluginID: "other", spec: contract.FuncSpec{Name: "second"}}
		h.mu.Unlock()
		p := &mutableMetaPlugin{meta: contract.Meta{
			ID: "atomic", Name: "A", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "first"}, {Name: "second"}}},
		}}
		if err := h.Register(p); err == nil {
			t.Fatal("register with a function conflict succeeded")
		}
		if h.HasPlugin("atomic") {
			t.Fatal("failed register committed the plugin")
		}
		if _, ok := h.Function("atomic", "first"); ok {
			t.Fatal("failed register left a ghost first function")
		}
	})
}

func TestReplaceValidatesProspectiveRegistry(t *testing.T) {
	t.Run("version downgrade", func(t *testing.T) {
		h := New[any](Options[any]{})
		old := &mutableMetaPlugin{meta: contract.Meta{
			ID: "provider", Name: "P", Version: "2",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
		}}
		dependent := &mutableMetaPlugin{meta: contract.Meta{
			ID: "dependent", Name: "D", Version: "1",
			Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "provider", Kind: contract.DepInit, MinVersion: "2"}}},
		}}
		if err := h.Register(old); err != nil {
			t.Fatal(err)
		}
		if err := h.Register(dependent); err != nil {
			t.Fatal(err)
		}
		downgrade := &mutableMetaPlugin{meta: contract.Meta{
			ID: "provider", Name: "P", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
		}}
		if err := h.Replace(downgrade); err == nil {
			t.Fatal("replace downgraded a required dependency")
		}
		if got := h.meta["provider"].Version; got != "2" {
			t.Fatalf("provider version = %q, want old version 2", got)
		}
		if out, err := h.Call(context.Background(), "provider", "ping", nil); err != nil || string(out) != `"ok"` {
			t.Fatalf("old provider call = %s, %v", out, err)
		}
		if !h.HasPlugin("dependent") {
			t.Fatal("dependent disappeared after refused replace")
		}
	})

	t.Run("slot withdrawal", func(t *testing.T) {
		slots := map[string]contract.SlotSpec{"enc": {ID: "enc"}}
		h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
			s, ok := slots[id]
			return s, ok
		}})
		old := &mutableMetaPlugin{meta: contract.Meta{
			ID: "provider", Name: "P", Version: "1", Slot: "enc",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
		}}
		dependent := &mutableMetaPlugin{meta: contract.Meta{
			ID: "dependent", Name: "D", Version: "1",
			Requires: contract.Requires{Deps: []contract.Dependency{{Slot: "enc", Kind: contract.DepInit}}},
		}}
		if err := h.Register(old); err != nil {
			t.Fatal(err)
		}
		if err := h.Register(dependent); err != nil {
			t.Fatal(err)
		}
		withdrawn := &mutableMetaPlugin{meta: contract.Meta{
			ID: "provider", Name: "P", Version: "2",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
		}}
		if err := h.Replace(withdrawn); err == nil {
			t.Fatal("replace withdrew the sole provider of a required slot")
		}
		if got := h.meta["provider"].Slot; got != "enc" {
			t.Fatalf("provider slot = %q, want old slot enc", got)
		}
		if out, err := h.Call(context.Background(), "provider", "ping", nil); err != nil || string(out) != `"ok"` {
			t.Fatalf("old provider call = %s, %v", out, err)
		}
	})
}

func TestDependencySlotPriorityIsConsistent(t *testing.T) {
	newHost := func(t *testing.T) *Host[any] {
		t.Helper()
		slots := map[string]contract.SlotSpec{"enc": {ID: "enc"}}
		h := New[any](Options[any]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
			s, ok := slots[id]
			return s, ok
		}})
		if err := h.Register(&mutableMetaPlugin{meta: contract.Meta{ID: "provider", Name: "P", Version: "1", Slot: "enc"}}); err != nil {
			t.Fatal(err)
		}
		return h
	}
	dual := contract.Dependency{Plugin: "missing.plugin", Slot: "enc", Kind: contract.DepInit, MinVersion: "99"}
	consumer := func(id string) *mutableMetaPlugin {
		return &mutableMetaPlugin{meta: contract.Meta{
			ID: id, Name: id, Version: "1",
			Requires: contract.Requires{Deps: []contract.Dependency{dual}},
		}}
	}

	t.Run("Register", func(t *testing.T) {
		h := newHost(t)
		if err := h.Register(consumer("direct")); err != nil {
			t.Fatalf("Slot-first dependency rejected: %v", err)
		}
	})
	t.Run("RegisterMany", func(t *testing.T) {
		h := newHost(t)
		rep := h.RegisterMany([]contract.Plugin{consumer("batch")})
		if len(rep.Failed) != 0 || len(rep.Registered) != 1 {
			t.Fatalf("RegisterMany = %+v, want Slot-first success", rep)
		}
	})
	t.Run("Remove", func(t *testing.T) {
		h := newHost(t)
		if err := h.Register(consumer("direct")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Remove("provider", false); err == nil {
			t.Fatal("Remove ignored the Slot half of a dual dependency")
		}
		if !h.HasPlugin("provider") {
			t.Fatal("refused Remove deleted the provider")
		}
	})
	t.Run("Replace", func(t *testing.T) {
		h := newHost(t)
		if err := h.Register(consumer("direct")); err != nil {
			t.Fatal(err)
		}
		if err := h.Replace(&mutableMetaPlugin{meta: contract.Meta{ID: "provider", Name: "P", Version: "2"}}); err == nil {
			t.Fatal("Replace ignored the Slot half of a dual dependency")
		}
		if got := h.meta["provider"].Slot; got != "enc" {
			t.Fatalf("provider slot = %q, want enc", got)
		}
	})
}

type replayPlugin struct {
	meta     contract.Meta
	applyErr error
	response string
}

func (p *replayPlugin) Meta() contract.Meta { return p.meta }
func (p *replayPlugin) ApplyConfig(json.RawMessage) error {
	return p.applyErr
}
func (p *replayPlugin) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(p.response)
}

func TestReplaceConfigReplayFailureKeepsOldGeneration(t *testing.T) {
	bus := NewMemEvents()
	h := New[any](Options[any]{Events: bus})
	var replaced atomic.Int32
	bus.Subscribe(&contract.EventFilter{Type: contract.EventPluginReplaced}, func(context.Context, contract.Event) {
		replaced.Add(1)
	})
	old := &replayPlugin{meta: contract.Meta{
		ID: "configured", Name: "C", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
	}, response: "old"}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("configured", json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	var oldEffectClosed atomic.Bool
	h.effects["configured"].Defer(func() { oldEffectClosed.Store(true) })

	bad := &replayPlugin{meta: contract.Meta{
		ID: "configured", Name: "C", Version: "2",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
	}, response: "new", applyErr: errors.New("rejected config")}
	if err := h.Replace(bad); err == nil {
		t.Fatal("Replace succeeded despite config replay failure")
	}
	if got := h.meta["configured"].Version; got != "1" {
		t.Fatalf("version = %q, want old version 1", got)
	}
	if out, err := h.Call(context.Background(), "configured", "ping", nil); err != nil || string(out) != `"old"` {
		t.Fatalf("old call = %s, %v", out, err)
	}
	if oldEffectClosed.Load() {
		t.Fatal("failed Replace closed the old effect")
	}
	if replaced.Load() != 0 {
		t.Fatal("failed Replace broadcast plugin.replaced")
	}

	good := &replayPlugin{meta: bad.meta, response: "new"}
	if err := h.Replace(good); err != nil {
		t.Fatalf("valid Replace failed: %v", err)
	}
	if !oldEffectClosed.Load() {
		t.Fatal("successful Replace did not close the old effect")
	}
	if replaced.Load() != 1 {
		t.Fatalf("plugin.replaced count = %d, want 1", replaced.Load())
	}
	if out, err := h.Call(context.Background(), "configured", "ping", nil); err != nil || string(out) != `"new"` {
		t.Fatalf("new call = %s, %v", out, err)
	}
}

type reentrantMetaPlugin struct {
	meta  contract.Meta
	host  *Host[any]
	calls atomic.Int32
}

func (p *reentrantMetaPlugin) Meta() contract.Meta {
	if p.calls.Add(1) > 1 {
		_ = p.host.Plugins()
	}
	return p.meta
}

func TestRetryPendingDoesNotCallMetaUnderHostLock(t *testing.T) {
	h := New[any](Options[any]{})
	p := &reentrantMetaPlugin{host: h, meta: contract.Meta{
		ID: "pending.meta", Name: "P", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "base", Kind: contract.DepInit}}},
	}}
	if status, err := h.RegisterLazy(p); err != nil || status != StatusPending {
		t.Fatalf("RegisterLazy = %v, %v", status, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- h.Register(&mutableMetaPlugin{meta: contract.Meta{ID: "base", Name: "B", Version: "1"}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryPending called external Meta while holding h.mu")
	}
	if !h.HasPlugin("pending.meta") {
		t.Fatal("ready pending plugin was not registered")
	}
}

func TestRemoveClosesEffectsOutsideHostLock(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(&mutableMetaPlugin{meta: contract.Meta{ID: "effect", Name: "E", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	h.effects["effect"].Defer(func() { _ = h.Plugins() })
	done := make(chan error, 1)
	go func() {
		_, err := h.Remove("effect", false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Remove executed an effect while holding h.mu")
	}
}
