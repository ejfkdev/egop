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

type leasePlugin struct {
	meta     contract.Meta
	response string
	toolName string
	started  chan struct{}
	release  chan struct{}
}

func (p *leasePlugin) Meta() contract.Meta { return p.meta }

func (p *leasePlugin) block() {
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	if p.release != nil {
		<-p.release
	}
}

func (p *leasePlugin) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	p.block()
	return json.Marshal(p.response)
}

func (p *leasePlugin) ToolSpecs() []contract.FuncSpec {
	if p.toolName == "" {
		return nil
	}
	return []contract.FuncSpec{{Name: p.toolName}}
}

func (p *leasePlugin) Tool(name string) (contract.ToolFunc[any], bool) {
	if name == "" || name != p.toolName {
		return nil, false
	}
	return func(context.Context, *any, json.RawMessage) (json.RawMessage, error) {
		p.block()
		return json.Marshal(p.response)
	}, true
}

func leaseMeta(id, version string) contract.Meta {
	return contract.Meta{
		ID: id, Name: id, Version: version,
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}},
	}
}

func waitForOldGenerationSeal(t *testing.T, h *Host[any], id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		g := h.generations[id]
		sealed := g != nil && g.sealed
		h.mu.Unlock()
		if sealed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("old generation for %s was not sealed", id)
}

func TestReplaceDrainsHostCallLeaseBeforeClosingOldEffect(t *testing.T) {
	h := New[any](Options[any]{ReplaceQuiesce: 2 * time.Second})
	old := &leasePlugin{meta: leaseMeta("lease.call", "1"), response: "old", started: make(chan struct{}, 1), release: make(chan struct{})}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}
	var oldEffectClosed atomic.Bool
	h.effects["lease.call"].Defer(func() { oldEffectClosed.Store(true) })

	callDone := make(chan error, 1)
	go func() {
		out, err := h.Call(context.Background(), "lease.call", "ping", nil)
		if err == nil && string(out) != `"old"` {
			err = errors.New("old call returned wrong response: " + string(out))
		}
		callDone <- err
	}()
	select {
	case <-old.started:
	case <-time.After(time.Second):
		t.Fatal("old Call did not start")
	}

	replacement := &leasePlugin{meta: leaseMeta("lease.call", "2"), response: "new"}
	replaceDone := make(chan error, 1)
	go func() { replaceDone <- h.Replace(replacement) }()
	waitForOldGenerationSeal(t, h, "lease.call")
	if _, err := h.Call(context.Background(), "lease.call", "ping", nil); err == nil {
		t.Fatal("old generation admitted a new Call after being sealed")
	}
	select {
	case err := <-replaceDone:
		t.Fatalf("Replace returned before the old Call drained: %v", err)
	default:
	}
	if oldEffectClosed.Load() {
		t.Fatal("Replace closed the old effect while Call was blocked")
	}

	close(old.release)
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old Call did not finish after release")
	}
	select {
	case err := <-replaceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Replace did not finish after the Call lease drained")
	}
	if !oldEffectClosed.Load() {
		t.Fatal("successful Replace did not close the old effect")
	}
	if out, err := h.Call(context.Background(), "lease.call", "ping", nil); err != nil || string(out) != `"new"` {
		t.Fatalf("new generation Call = %s, %v", out, err)
	}
}

func TestReplaceDrainsCollectedToolRunLease(t *testing.T) {
	h := New[any](Options[any]{ReplaceQuiesce: 2 * time.Second})
	meta := leaseMeta("lease.tool", "1")
	meta.Provides.Capabilities = []string{contract.CapTools}
	old := &leasePlugin{meta: meta, response: "old-tool", toolName: "work", started: make(chan struct{}, 1), release: make(chan struct{})}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}
	tools := h.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(tools))
	}
	tool := tools[0]
	var oldEffectClosed atomic.Bool
	h.effects["lease.tool"].Defer(func() { oldEffectClosed.Store(true) })

	runDone := make(chan error, 1)
	go func() {
		out, err := tool.Run(context.Background(), nil, nil)
		if err == nil && out != `"old-tool"` {
			err = errors.New("old tool returned wrong response: " + out)
		}
		runDone <- err
	}()
	select {
	case <-old.started:
	case <-time.After(time.Second):
		t.Fatal("old Tool.Run did not start")
	}

	newMeta := leaseMeta("lease.tool", "2")
	replacement := &leasePlugin{meta: newMeta, response: "new"}
	replaceDone := make(chan error, 1)
	go func() { replaceDone <- h.Replace(replacement) }()
	waitForOldGenerationSeal(t, h, "lease.tool")
	if _, err := tool.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("collected old Tool admitted a new Run after being sealed")
	}
	select {
	case err := <-replaceDone:
		t.Fatalf("Replace returned before the old Tool.Run drained: %v", err)
	default:
	}
	if oldEffectClosed.Load() {
		t.Fatal("Replace closed the old effect while Tool.Run was blocked")
	}

	close(old.release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old Tool.Run did not finish after release")
	}
	select {
	case err := <-replaceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Replace did not finish after the Tool.Run lease drained")
	}
	if !oldEffectClosed.Load() {
		t.Fatal("successful Replace did not close the old effect")
	}
	if _, err := tool.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("collected old Tool remained usable after Replace committed")
	}
}

func TestRemoveDrainsHostCallLeaseBeforeClosingOldEffect(t *testing.T) {
	h := New[any](Options[any]{RemoveQuiesce: 2 * time.Second})
	old := &leasePlugin{meta: leaseMeta("lease.remove", "1"), response: "old", started: make(chan struct{}, 1), release: make(chan struct{})}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}
	var oldEffectClosed atomic.Bool
	h.effects["lease.remove"].Defer(func() { oldEffectClosed.Store(true) })

	callDone := make(chan error, 1)
	go func() {
		_, err := h.Call(context.Background(), "lease.remove", "ping", nil)
		callDone <- err
	}()
	select {
	case <-old.started:
	case <-time.After(time.Second):
		t.Fatal("old Call did not start")
	}
	removeDone := make(chan error, 1)
	go func() {
		_, err := h.Remove("lease.remove", false)
		removeDone <- err
	}()
	waitForOldGenerationSeal(t, h, "lease.remove")
	if _, err := h.Call(context.Background(), "lease.remove", "ping", nil); err == nil {
		t.Fatal("sealed Remove generation admitted a new Call")
	}
	select {
	case err := <-removeDone:
		t.Fatalf("Remove returned before the old Call drained: %v", err)
	default:
	}
	if oldEffectClosed.Load() {
		t.Fatal("Remove closed the old effect while Call was blocked")
	}
	close(old.release)
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old Call did not finish")
	}
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove did not finish after the Call lease drained")
	}
	if !oldEffectClosed.Load() || h.HasPlugin("lease.remove") {
		t.Fatal("successful Remove did not commit cleanup")
	}
}

func TestQuiesceTimeoutKeepsOldGenerationAndCanRetry(t *testing.T) {
	for _, operation := range []string{"replace", "remove"} {
		t.Run(operation, func(t *testing.T) {
			h := New[any](Options[any]{ReplaceQuiesce: 40 * time.Millisecond, RemoveQuiesce: 40 * time.Millisecond})
			id := "lease.timeout." + operation
			old := &leasePlugin{meta: leaseMeta(id, "1"), response: "old", started: make(chan struct{}, 1), release: make(chan struct{})}
			if err := h.Register(old); err != nil {
				t.Fatal(err)
			}
			var oldEffectClosed atomic.Bool
			h.effects[id].Defer(func() { oldEffectClosed.Store(true) })
			callDone := make(chan error, 1)
			go func() {
				_, err := h.Call(context.Background(), id, "ping", nil)
				callDone <- err
			}()
			select {
			case <-old.started:
			case <-time.After(time.Second):
				t.Fatal("old Call did not start")
			}

			operationDone := make(chan error, 1)
			if operation == "replace" {
				go func() { operationDone <- h.Replace(&leasePlugin{meta: leaseMeta(id, "2"), response: "new"}) }()
			} else {
				go func() {
					_, err := h.Remove(id, false)
					operationDone <- err
				}()
			}
			select {
			case err := <-operationDone:
				if !errors.Is(err, ErrQuiesceTimeout) {
					t.Fatalf("%s error = %v, want ErrQuiesceTimeout", operation, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s did not observe its quiesce deadline", operation)
			}
			if oldEffectClosed.Load() || !h.HasPlugin(id) {
				t.Fatalf("timed out %s closed effect or removed old plugin", operation)
			}
			if got := h.meta[id].Version; got != "1" {
				t.Fatalf("timed out %s changed version to %s", operation, got)
			}

			close(old.release)
			select {
			case err := <-callDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("old Call did not finish")
			}
			// A timed-out transaction must resume admission on the old generation.
			if out, err := h.Call(context.Background(), id, "ping", nil); err != nil || string(out) != `"old"` {
				t.Fatalf("old generation did not resume admission: %s, %v", out, err)
			}
			if operation == "replace" {
				if err := h.Replace(&leasePlugin{meta: leaseMeta(id, "2"), response: "new"}); err != nil {
					t.Fatalf("Replace retry failed: %v", err)
				}
			} else {
				if _, err := h.Remove(id, false); err != nil {
					t.Fatalf("Remove retry failed: %v", err)
				}
			}
			if !oldEffectClosed.Load() {
				t.Fatalf("successful %s retry did not close old effect", operation)
			}
		})
	}
}
