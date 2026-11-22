package host_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
	wasm "github.com/ejfkdev/egop/loader/wasm"
)

// wasmCallOnly 刻意不暴露 Busy：真实 wazero 实例的 Replace 等待只能由
// Host generation lease 驱动，不能由 wasm.Plugin.Quiescer 代答。
type wasmCallOnly struct{ p *wasm.Plugin }

func (w wasmCallOnly) Meta() contract.Meta           { return w.p.Meta() }
func (w wasmCallOnly) SetSurface(s contract.Surface) { w.p.SetSurface(s) }
func (w wasmCallOnly) CallFunc(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	return w.p.CallFunc(ctx, name, input)
}

type blockingMath struct {
	started chan struct{}
	release chan struct{}
}

func (p blockingMath) Meta() contract.Meta {
	return contract.Meta{
		ID: "dummy.math", Name: "Math", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "add"}}},
	}
}

func (p blockingMath) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return json.RawMessage(`3`), nil
}

func TestRealWasmReplaceWaitsForHostCallLease(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "loader", "wasm", "testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := wasm.LoadFS(context.Background(), data, "demo.egop.wasm", wasm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	newRaw, err := wasm.LoadFS(context.Background(), data, "demo.egop.wasm", wasm.Options{})
	if err != nil {
		_ = oldRaw.Close(context.Background())
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		_ = oldRaw.Close(context.Background())
		_ = newRaw.Close(context.Background())
	})

	h := host.New[any](host.Options[any]{ReplaceQuiesce: 2 * time.Second})
	if err := h.Register(blockingMath{started: started, release: release}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(wasmCallOnly{p: oldRaw}); err != nil {
		t.Fatal(err)
	}

	callDone := make(chan error, 1)
	go func() {
		out, err := h.Call(context.Background(), "wasm.demo", "call", json.RawMessage(`{}`))
		if err == nil && string(out) != "3" {
			err = &unexpectedWasmResult{got: string(out)}
		}
		callDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("real WASM nested call did not enter the blocking host function")
	}

	replaceDone := make(chan error, 1)
	go func() { replaceDone <- h.Replace(wasmCallOnly{p: newRaw}) }()
	select {
	case err := <-replaceDone:
		t.Fatalf("Replace returned while the real WASM call was blocked: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	unblock()
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("real WASM call did not finish after release")
	}
	select {
	case err := <-replaceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Replace did not finish after the real WASM lease drained")
	}
}

type unexpectedWasmResult struct{ got string }

func (e *unexpectedWasmResult) Error() string { return "unexpected real WASM result: " + e.got }
