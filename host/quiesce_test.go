package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
)

// busyPlug 可编程 Quiescer(测等待语义)。
type busyPlug struct {
	meta   contract.Meta
	probed chan struct{}

	mu     sync.Mutex
	busy   bool
	called int
}

func (b *busyPlug) Meta() contract.Meta { return b.meta }
func (b *busyPlug) Busy() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called++
	if b.probed != nil {
		select {
		case b.probed <- struct{}{}:
		default:
		}
	}
	return b.busy, nil
}

func (b *busyPlug) setBusy(busy bool) {
	b.mu.Lock()
	b.busy = busy
	b.mu.Unlock()
}

func (b *busyPlug) probeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.called
}

func (b *busyPlug) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

type busyErrPlug struct{ meta contract.Meta }

func (b *busyErrPlug) Meta() contract.Meta { return b.meta }
func (b *busyErrPlug) Busy() (bool, error) { return false, errors.New("probe unavailable") }

// cfgPlug 带配置与 Secret 键(测 Diagnostics 脱敏)。
type cfgPlug struct {
	meta contract.Meta
}

func (c *cfgPlug) Meta() contract.Meta                   { return c.meta }
func (c *cfgPlug) ApplyConfig(cfg json.RawMessage) error { return nil }
func (c *cfgPlug) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

// 超时等待语义：deadline 只管等待方，
// 结果可重复 join;无探测面插件零等待(兼容旧语义)。
func TestAwaitQuiesceTimeoutSemantics(t *testing.T) {
	h := New[struct{}](Options[struct{}]{})
	bp := &busyPlug{busy: true, meta: contract.Meta{ID: "demo.busy", Name: "Busy", Version: "1", Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}}}}
	if err := h.Register(bp); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	quiet, err := h.awaitQuiesce(nil, "demo.busy", 80*time.Millisecond)
	if quiet || err != nil {
		t.Fatalf("busy plugin: quiet=%v err=%v", quiet, err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("waiter returned too early: %v", time.Since(start))
	}
	bp.setBusy(false)
	if quiet, err := h.awaitQuiesce(nil, "demo.busy", time.Second); !quiet || err != nil {
		t.Fatalf("quiet plugin: quiet=%v err=%v", quiet, err)
	}
	if n := bp.probeCount(); n < 3 {
		t.Fatalf("probe count = %d, want >=3 (repeat-join)", n)
	}
	plain := &cfgPlug{meta: contract.Meta{ID: "demo.plain", Name: "Plain", Version: "1"}}
	if err := h.Register(plain); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if quiet, err := h.awaitQuiesce(nil, "demo.plain", time.Second); quiet || err != nil {
		t.Fatalf("non-quiescer: quiet=%v err=%v", quiet, err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("non-quiescer must not wait, took %v", time.Since(start))
	}
}

func TestReplaceUnavailableProbeDegradesToNoProbe(t *testing.T) {
	h := New[struct{}](Options[struct{}]{ReplaceQuiesce: time.Second, RemoveQuiesce: time.Second})
	old := &busyErrPlug{meta: contract.Meta{ID: "demo.probe-error", Name: "old", Version: "1"}}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := h.Replace(&busyErrPlug{meta: contract.Meta{ID: "demo.probe-error", Name: "new", Version: "2"}}); err != nil {
		t.Fatalf("unavailable probe must degrade to no-probe semantics Replace must not fail on unavailable Busy probe: %v", err)
	}
	if _, err := h.Remove("demo.probe-error", false); err != nil {
		t.Fatalf("unavailable probe must degrade to no-probe semantics Remove must not fail on unavailable Busy probe: %v", err)
	}
}

func TestReplaceQuiescesOldInstance(t *testing.T) {
	h := New[struct{}](Options[struct{}]{ReplaceQuiesce: time.Second})
	meta := func(version string) contract.Meta {
		return contract.Meta{ID: "demo.replace", Name: "Replace", Version: version}
	}
	old := &busyPlug{meta: meta("1"), busy: true, probed: make(chan struct{}, 1)}
	replacement := &busyPlug{meta: meta("2"), busy: false}
	if err := h.Register(old); err != nil {
		t.Fatal(err)
	}

	replaced := make(chan error, 1)
	go func() { replaced <- h.Replace(replacement) }()
	select {
	case <-old.probed:
		// The old instance was probed while still busy; Replace must remain blocked.
	case err := <-replaced:
		t.Fatalf("Replace returned after probing the idle replacement instead of waiting for the busy old instance: %v", err)
	case <-time.After(time.Second):
		t.Fatal("old instance was never probed")
	}
	select {
	case err := <-replaced:
		t.Fatalf("Replace returned while the old instance was still busy: %v", err)
	default:
	}
	if n := replacement.probeCount(); n != 0 {
		t.Fatalf("replacement was probed %d times; quiesce must target only the old instance", n)
	}

	old.setBusy(false)
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Replace did not finish after the old instance became idle")
	}
}

// Diagnostics 快照:插件/函数/配置脱敏/busy 探测齐备。
func TestDiagnosticsSnapshot(t *testing.T) {
	h := New[struct{}](Options[struct{}]{})
	cp := &cfgPlug{meta: contract.Meta{ID: "demo.diag", Name: "Diag", Version: "1", Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "ping"}}, Config: []contract.ConfigFieldSpec{{Key: "token", Secret: true}, {Key: "enabled"}}}}}
	if err := h.Register(cp); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("demo.diag", json.RawMessage(`{"enabled":true,"token":"SECRET-TOKEN-XYZ"}`)); err != nil {
		t.Fatal(err)
	}
	d := h.Diagnostics()
	var dp *DiagPlugin
	for i := range d.Plugins {
		if d.Plugins[i].ID == "demo.diag" {
			dp = &d.Plugins[i]
		}
	}
	if dp == nil {
		t.Fatal("demo.diag missing from snapshot")
	}
	if dp.Config == nil {
		t.Fatal("config missing in diag")
	}
	cs := string(dp.Config)
	if strings.Contains(cs, "SECRET-TOKEN-XYZ") {
		t.Fatalf("secret leaked in diagnostics: %s", cs)
	}
	if !strings.Contains(cs, "enabled") {
		t.Fatalf("non-secret config dropped: %s", cs)
	}
	if len(dp.DepsUnmet) != 0 {
		t.Fatalf("registered plugin should have no unmet deps: %v", dp.DepsUnmet)
	}
	found := false
	for _, f := range d.Fns {
		if f.Plugin == "demo.diag" && f.Name == "ping" {
			found = true
		}
	}
	if !found {
		t.Fatal("functions missing in diag")
	}
}
