// examples/codeless 冒烟:无代码资源包(纯清单/资产)装载、注册、Extensions/Assets
// 消费、无代码面 fail-closed;品牌后缀经 ExtraSuffixes 注入、未注入即忽略。
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/loader/wasm"
)

const testPackManifest = `{"id":"demo.pack","name":"Resource Pack","version":"1.0.0",` +
	`"extensions":{"demo.entry":{"file":"greeting.txt","kind":"text"}}}`

func TestExampleCodeless(t *testing.T) {
	ctx := context.Background()
	pack := buildPack(testPackManifest, map[string]string{
		"greeting.txt":   "hello from a codeless bundle",
		"theme/dark.css": "body{background:#111}",
	})
	fsys := fstest.MapFS{"plugins/demo.pack.zip": {Data: pack}}

	// 未注入后缀:忽略(内容无关库不认品牌词)。
	if plugs, errs := wasm.ScanFS(ctx, fsys, wasm.Options{}); len(plugs) != 0 || len(errs) != 0 {
		t.Fatalf("un-injected suffix: plugs=%d errs=%v", len(plugs), errs)
	}

	// 注入后:发现并装载。
	plugs, errs := wasm.ScanFS(ctx, fsys, wasm.Options{ExtraSuffixes: []string{".pack.zip"}})
	if len(errs) != 0 || len(plugs) != 1 {
		t.Fatalf("ScanFS plugs=%d errs=%v", len(plugs), errs)
	}
	p := plugs[0]
	defer p.Close(ctx)

	if p.Meta().ID != "demo.pack" {
		t.Fatalf("meta id = %s", p.Meta().ID)
	}
	// Extensions 原样透传,消费方按 key 读。
	raw, ok := p.Meta().Extensions["demo.entry"]
	if !ok {
		t.Fatal("extensions demo.entry missing")
	}
	var entry struct {
		File string `json:"file"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.File != "greeting.txt" {
		t.Fatalf("entry = %+v (%v)", entry, err)
	}
	// Assets:含子目录资源。
	if got := string(p.Assets()[entry.File]); got != "hello from a codeless bundle" {
		t.Fatalf("asset = %q", got)
	}
	if got := string(p.Assets()["theme/dark.css"]); !strings.Contains(got, "background") {
		t.Fatalf("nested asset = %q", got)
	}

	// 注册进宿主:同一生命周期面。
	h := host.New[any](host.Options[any]{})
	if err := h.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !h.HasPlugin("demo.pack") {
		t.Fatal("codeless bundle not registered")
	}
	// 无代码面 fail-closed:干净错误而非 panic。
	if _, err := h.Call(ctx, "demo.pack", "anything", nil); err == nil {
		t.Fatal("call on codeless should error")
	}
	if err := h.SetConfig("demo.pack", json.RawMessage(`{"k":1}`)); err == nil ||
		!strings.Contains(err.Error(), "not configurable") {
		t.Fatalf("config on codeless err = %v", err)
	}
	// 卸载照常。
	if _, err := h.Remove("demo.pack", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if h.HasPlugin("demo.pack") {
		t.Fatal("still registered after remove")
	}
}
