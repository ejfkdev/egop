// 热更装载器测试:注册/替换/移除/坏包回退/配置重放/两段确认防抖。
package autoload

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/loader"
)

const demoManifest = `{"id":"wasm.demo","name":"WASM Demo","version":"1.0.0","provides":{"capabilities":["plugin.call","event.listen","storage.kv","tool.provide"],"functions":[{"name":"add"},{"name":"call"},{"name":"kv"}]}}`

func coreHost(t *testing.T) loader.HostFace {
	t.Helper()
	return host.New[any](host.Options[any]{})
}

func demoModule(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// paddedModule 返回合法 wasm(尾部追加自定义段,hash 变、清单不变)。wazero 1.12
// 对"零载荷自定义段"会按剩余字节再读一下而报 EOF,故带 1 字节载荷。
func paddedModule(t *testing.T) []byte {
	t.Helper()
	base := demoModule(t)
	pad := []byte{0x00, 0x05, 0x03, 'p', 'a', 'd', 0x00} // 自定义段:名 pad + 1 字节载荷
	return append(append([]byte{}, base...), pad...)
}

func writeZip(t *testing.T, dir, name, manifest string, module []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(manifest))
	w, err = zw.Create("plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(module)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// settle 轮询至无新动作或上限(两段确认 → 多数场景 3 轮内落定)。
func settle(t *testing.T, w *Watcher, rounds int) []Event {
	t.Helper()
	var all []Event
	for i := 0; i < rounds; i++ {
		evs := w.Poll(context.Background())
		all = append(all, evs...)
	}
	return all
}

func hasAction(t *testing.T, evs []Event, want Action) bool {
	t.Helper()
	for _, e := range evs {
		if e.Action == want {
			return true
		}
	}
	return false
}

func TestPollRegisterAndRemove(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	p := writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))

	w := New(h, []string{dir}, Options{})
	evs := settle(t, w, 3)
	if !hasAction(t, evs, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("register events=%v plugins=%v", evs, h.Plugins())
	}

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	evs = settle(t, w, 2)
	if !hasAction(t, evs, ActionRemove) || h.HasPlugin("wasm.demo") {
		t.Fatalf("remove events=%v still=%v", evs, h.HasPlugin("wasm.demo"))
	}
}

func TestPollReplaceKeepsOldOnBad(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	p := writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("initial register failed")
	}

	if err := os.WriteFile(p, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs := settle(t, w, 3)
	var fail bool
	for _, e := range evs {
		if e.Action == ActionFailed && e.Err != nil {
			fail = true
		}
	}
	if !fail || !h.HasPlugin("wasm.demo") {
		t.Fatalf("want failed reload with old kept; events=%v has=%v", evs, h.HasPlugin("wasm.demo"))
	}
}

func TestPollReplaceAndConfigReplay(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	p := writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)

	cfg := json.RawMessage(`{"k":1}`)
	if err := h.SetConfig("wasm.demo", cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	// 内容变清单不变 → 热替换
	writeZip(t, dir, "demo.egop.zip", demoManifest, paddedModule(t))
	evs := settle(t, w, 3)
	if !hasAction(t, evs, ActionReplace) {
		t.Fatalf("no replace: %v", evs)
	}
	got, ok := h.AppliedConfig("wasm.demo")
	if !ok || string(got) != string(cfg) {
		t.Fatalf("config not replayed: got=%s ok=%v", got, ok)
	}
	if _, err := h.Call(context.Background(), "wasm.demo", "add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("old version not serving: %v", err)
	}
	_ = p
}

func TestPollTwoRoundConfirm(t *testing.T) {
	// 两段确认:内容不稳时不装载(半截写入/连续覆盖各一轮才应用)。
	h := coreHost(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "demo.egop.zip")
	if err := os.WriteFile(p, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := New(h, []string{dir}, Options{})
	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("round1 should not apply: %v", evs)
	}
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("round2 should still confirm: %v", evs)
	}
	evs := w.Poll(context.Background())
	if !hasAction(t, evs, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("round3 should apply: %v", evs)
	}
}

func TestPollReidentify(t *testing.T) {
	// 同路径换 id:新 id 进册 + 旧 id 卸下。
	h := coreHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)

	newManifest := `{"id":"wasm.sec","name":"S","version":"2","provides":{"functions":[{"name":"add"}]}}`
	writeZip(t, dir, "demo.egop.zip", newManifest, demoModule(t))
	evs := settle(t, w, 3)
	if !h.HasPlugin("wasm.sec") || h.HasPlugin("wasm.demo") {
		t.Fatalf("re-identify failed: %v plugins=%v", evs, h.Plugins())
	}
}

func TestStopAfterStartClosesStopChannel(t *testing.T) {
	// Start 与 Stop 若共用一个 sync.Once,Start 会 consume 掉它使 Stop 变成 no-op;
	// 这里断言 Stop 在 Start 之后仍能关闭 stop 通道(否则热更 goroutine 泄漏)。
	w := New(nil, nil, Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.Stop()
	select {
	case <-w.stop:
	default:
		t.Fatal("Stop() after Start() did not close the stop channel")
	}
}

func TestPollFromInjectedFS(t *testing.T) {
	// 注入的只读 FS(浏览器/内嵌场景):dirs 视作 FS 内的根,热更引擎照常工作。
	h := coreHost(t)
	fsys := fstest.MapFS{
		"plugins/demo.egop.wasm": &fstest.MapFile{Data: demoModule(t)},
	}
	w := New(h, []string{"plugins"}, Options{FS: fsys})
	evs := settle(t, w, 3)
	if !hasAction(t, evs, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("register events=%v plugins=%v", evs, h.Plugins())
	}
}

func TestPollLoadsLateDependency(t *testing.T) {
	// 依赖方先出现、被依赖方后到:后到者落地后,依赖方应在后续轮次自动补载。
	h := coreHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "b.egop.zip", `{"id":"wasm.b","name":"B","version":"1","requires":{"deps":[{"plugin":"wasm.a","kind":"init"}]}}`, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 4)
	if h.HasPlugin("wasm.b") {
		t.Fatal("B should not register before its dependency A arrives")
	}

	writeZip(t, dir, "a.egop.zip", `{"id":"wasm.a","name":"A","version":"1"}`, demoModule(t))
	evs := settle(t, w, 8)
	if !h.HasPlugin("wasm.a") || !h.HasPlugin("wasm.b") {
		t.Fatalf("A then B should both load: evs=%v plugins=%v", evs, h.Plugins())
	}
}
