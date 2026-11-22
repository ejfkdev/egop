// 热更装载器测试:注册/替换/移除/坏包回退/配置重放/两段确认防抖。
package autoload

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ejfkdev/egop/contract"
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
	// 删除也是两段确认:首轮未见只记候选,不卸载(抗目录瞬态读失败/文件系统抖动)。
	evs = w.Poll(context.Background())
	if hasAction(t, evs, ActionRemove) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("removal must wait for a second confirming round: events=%v", evs)
	}
	evs = w.Poll(context.Background())
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
	if extra := settle(t, w, 4); len(extra) != 0 {
		t.Fatalf("unchanged failed replacement must not retry LoadFS: %v", extra)
	}
	if _, err := h.Call(context.Background(), "wasm.demo", "add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("old version must remain usable while the bad replacement is suppressed: %v", err)
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

type changingReadFS struct {
	fs.FS
	first []byte
	later []byte
	reads int
}

func (f *changingReadFS) ReadFile(string) ([]byte, error) {
	f.reads++
	if f.reads <= 2 {
		return bytes.Clone(f.first), nil
	}
	return bytes.Clone(f.later), nil
}

func TestPollRestartsConfirmationWhenLoadBytesDiffer(t *testing.T) {
	first, later := demoModule(t), paddedModule(t)
	mfs := fstest.MapFS{
		"plugins/demo.egop.wasm": &fstest.MapFile{Data: first}, // zero mtime: hash every poll
	}
	scripted := &changingReadFS{FS: mfs, first: first, later: later}
	h := coreHost(t)
	w := New(h, []string{"plugins"}, Options{FS: scripted})
	defer w.Unload(context.Background())

	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("first confirmation events = %v", evs)
	}
	evs := w.Poll(context.Background()) // A/A confirmed, but the load read returns B.
	if len(evs) != 1 || evs[0].Action != ActionFailed || evs[0].Err == nil ||
		!strings.Contains(evs[0].Err.Error(), "content changed") {
		t.Fatalf("mismatched load bytes must be rejected: %v", evs)
	}
	if h.HasPlugin("wasm.demo") {
		t.Fatal("changed bytes must not register under the previously confirmed hash")
	}
	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("replacement bytes need a fresh first confirmation: %v", evs)
	}
	evs = w.Poll(context.Background())
	if !hasAction(t, evs, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("stable replacement bytes should register after reconfirmation: %v", evs)
	}
	_, appliedHash, ok := w.ArtifactState("wasm.demo")
	if !ok || appliedHash != hashBytes(later) {
		t.Fatalf("applied hash = %q, want hash of bytes passed to LoadFS", appliedHash)
	}
}

func TestPollReidentifyRetriesWhenDependencyArrives(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	defer w.Unload(context.Background())

	newManifest := `{"id":"wasm.sec","name":"S","version":"2","requires":{"deps":[{"plugin":"wasm.dep","kind":"init"}]}}`
	writeZip(t, dir, "demo.egop.zip", newManifest, demoModule(t))
	evs := settle(t, w, 2)
	if !hasAction(t, evs, ActionFailed) || !h.HasPlugin("wasm.demo") || h.HasPlugin("wasm.sec") {
		t.Fatalf("missing dependency must preserve the old plugin: events=%v plugins=%v", evs, h.Plugins())
	}
	key := filepath.Join(dir, "demo.egop.zip")
	u := w.units[key]
	if u == nil || u.np == nil {
		t.Fatalf("re-identification candidate was not retained: %+v", u)
	}
	candidate := u.np
	writeZip(t, dir, "dep.egop.zip", `{"id":"wasm.dep","name":"D","version":"1"}`, demoModule(t))
	evs = settle(t, w, 8)
	if !h.HasPlugin("wasm.dep") || !h.HasPlugin("wasm.sec") || h.HasPlugin("wasm.demo") {
		t.Fatalf("candidate should apply after its dependency arrives: events=%v plugins=%v", evs, h.Plugins())
	}
	if u.plugin != candidate || u.np != nil {
		t.Fatal("retry must install the retained re-identification candidate")
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

// TestExtraSuffixesDiscovery 品牌/项目自有 zip 后缀经 Options.ExtraSuffixes 注入:
// 注入后目录发现+装载贯通;未注入时同一文件被忽略(内容无关库不内置业务词)。
func TestExtraSuffixesDiscovery(t *testing.T) {
	dir := t.TempDir()
	writeZip(t, dir, "demo.brand.zip", demoManifest, demoModule(t))

	// 未注入:忽略(不注册、无事件)。
	w0 := New(coreHost(t), []string{dir}, Options{})
	if evs := settle(t, w0, 3); len(evs) != 0 {
		t.Fatalf("un-injected suffix must be ignored, events = %v", evs)
	}

	// 注入 ".brand.zip":两段确认后照常装载。
	h := coreHost(t)
	w := New(h, []string{dir}, Options{ExtraSuffixes: []string{".brand.zip"}})
	evs := settle(t, w, 3)
	if !hasAction(t, evs, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("injected suffix not loaded: events=%v plugins=%v", evs, h.Plugins())
	}
	w.Stop()
	w.Unload(context.Background())
}

// TestUnloadUnregistersAndCloses Watcher.Unload(mount 装配失败全清路径):
// 已加载插件全部反注册、句柄关闭;幂等;正常 Stop 不触发这些。
func TestUnloadUnregistersAndCloses(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("initial register failed")
	}
	// Stop 只停轮询,注册面不动(归宿主总闸)。
	w.Stop()
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("Stop must not unregister")
	}
	// Unload:反注册 + 关句柄(Close 后实例 broken,再调用 fail-closed)。
	w.Unload(context.Background())
	if h.HasPlugin("wasm.demo") {
		t.Fatal("Unload must unregister loaded plugins")
	}
	w.mu.Lock()
	remaining := len(w.units)
	w.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("units left after Unload = %d", remaining)
	}
	// 幂等:再调是空操作。
	w.Unload(context.Background())
}

// TestFailedContentNotRetriedUntilChange 内容坏件(非依赖问题)不再每轮重编译:
// 同一 hash 只试一次;内容变化后重新两段确认再试一次。
func TestFailedContentNotRetriedUntilChange(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.egop.wasm")
	if err := os.WriteFile(bad, []byte("not-wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := New(h, []string{dir}, Options{})
	// 前三轮:两段确认 + 一次装载尝试 → 恰一条 ActionFailed。
	evs := settle(t, w, 3)
	if n := countFailed(evs); n != 1 {
		t.Fatalf("rounds 1-3: failed events = %d (want 1), evs=%v", n, evs)
	}
	// 后续多轮:hash 未变 → 不再重试(无新事件、不重编译)。
	if evs := settle(t, w, 5); len(evs) != 0 {
		t.Fatalf("unchanged bad content must not retry, evs=%v", evs)
	}
	// 内容变化(仍是坏件):重新两段确认后再试一次。
	if err := os.WriteFile(bad, []byte("still-not-wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs = settle(t, w, 4)
	if n := countFailed(evs); n != 1 {
		t.Fatalf("after change: failed events = %d (want 1), evs=%v", n, evs)
	}
}

func countFailed(evs []Event) int {
	n := 0
	for _, e := range evs {
		if e.Action == ActionFailed {
			n++
		}
	}
	return n
}

// TestRegisterFailureKeepsInstance 依赖未就位:装载成功、注册失败——实例保留
// (np),后续轮次只重试 Register(同一实例指针,不重新 LoadFS);依赖到位后
// 注册成功的正是保留的实例。
func TestRegisterFailureKeepsInstance(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "b.egop.zip",
		`{"id":"wasm.b","name":"B","version":"1","requires":{"deps":[{"plugin":"wasm.a","kind":"init"}]}}`,
		demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 4)
	key := filepath.Join(dir, "b.egop.zip")
	u := w.units[key]
	if u == nil || u.np == nil {
		t.Fatalf("failed register must keep the loaded instance for Register-only retry, u=%+v", u)
	}
	np := u.np
	// 再来几轮:仍是同一实例(不重新装载)。
	settle(t, w, 3)
	if u.np != np {
		t.Fatal("kept instance must not be recreated across polls")
	}
	// 依赖落地 → 注册成功,且就是保留的实例。
	writeZip(t, dir, "a.egop.zip", `{"id":"wasm.a","name":"A","version":"1"}`, demoModule(t))
	settle(t, w, 8)
	if !h.HasPlugin("wasm.b") {
		t.Fatalf("B must register after dependency arrives, plugins=%v", h.Plugins())
	}
	if u.plugin != np || u.np != nil {
		t.Fatal("registered instance must be the kept np (no reload)")
	}
	w.Stop()
	w.Unload(context.Background())
}

func TestPollDeleteDiscardsFailedUnitAndClosesPendingInstance(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	path := writeZip(t, dir, "b.egop.zip",
		`{"id":"wasm.b","name":"B","version":"1","requires":{"deps":[{"plugin":"wasm.a","kind":"init"}]}}`,
		demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	key := filepath.Join(dir, "b.egop.zip")
	u := w.units[key]
	if u == nil || u.np == nil {
		t.Fatalf("failed registration should hold a pending instance: %+v", u)
	}
	np := u.np
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.Poll(context.Background())
	if got := w.units[key]; got == nil || got.np != np {
		t.Fatal("first missing round must retain the failed unit")
	}
	w.Poll(context.Background())
	if got := w.units[key]; got != nil {
		t.Fatalf("second missing round must discard failed unit: %+v", got)
	}
	if _, err := np.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err == nil {
		t.Fatal("pending instance was not closed after artifact deletion")
	}
	if h.HasPlugin("wasm.b") {
		t.Fatal("failed registration must remain absent after deletion")
	}
}

func TestPollDeleteDiscardsUnappliedPendingUnit(t *testing.T) {
	h := coreHost(t)
	dir := t.TempDir()
	path := writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	w.Poll(context.Background()) // 只记录候选 hash,尚未装载。
	key := filepath.Join(dir, "demo.egop.zip")
	if u := w.units[key]; u == nil || u.loaded || u.pending == "" {
		t.Fatalf("expected unapplied pending unit: %+v", w.units[key])
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.Poll(context.Background())
	if w.units[key] == nil {
		t.Fatal("first missing round discarded pending unit too early")
	}
	w.Poll(context.Background())
	if w.units[key] != nil {
		t.Fatal("second missing round did not discard pending unit")
	}
}

// countFS 记录 Open 次数(Stat 不计):锁住「未变文件不重读内容」的轮询优化。
type countFS struct {
	fs.FS
	opens *int32
}

func (c countFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".wasm") || strings.HasSuffix(name, ".zip") {
		atomic.AddInt32(c.opens, 1) // 只计插件包体;目录 readdir 不算
	}
	return c.FS.Open(name)
}

func (c countFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(c.FS, name) }

// TestPollSkipsUnchangedContent 内容签名未变时轮询只 stat、不读包体:
// 此前每秒全量 sha256 几十 MB 是空闲 CPU 的主要来源。
func TestPollSkipsUnchangedContent(t *testing.T) {
	h := coreHost(t)
	mfs := fstest.MapFS{
		"plugins/demo.egop.wasm": &fstest.MapFile{Data: demoModule(t), ModTime: time.Now()},
	}
	var opens int32
	w := New(h, []string{"plugins"}, Options{FS: countFS{FS: mfs, opens: &opens}})
	settle(t, w, 3)
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("demo should register")
	}
	base := atomic.LoadInt32(&opens)
	for i := 0; i < 3; i++ {
		w.Poll(context.Background())
	}
	if got := atomic.LoadInt32(&opens); got != base {
		t.Fatalf("unchanged file re-read: opens %d -> %d", base, got)
	}

	// 内容变化(size+mtime 变):变的那一份才被读;两段确认后替换。
	mfs["plugins/demo.egop.wasm"] = &fstest.MapFile{Data: paddedModule(t), ModTime: time.Now().Add(time.Second)}
	w.Poll(context.Background())
	if got := atomic.LoadInt32(&opens); got != base+1 {
		t.Fatalf("changed file should read once: opens %d (base %d)", got, base)
	}
	evs := w.Poll(context.Background())
	if !hasAction(t, evs, ActionReplace) {
		t.Fatalf("changed file should replace after confirm: %v", evs)
	}
}

type gatedReadsFS struct {
	fs.FS
	reads   atomic.Int32
	first   chan struct{}
	release chan struct{}
}

func (g *gatedReadsFS) Open(name string) (fs.File, error) { return g.FS.Open(name) }
func (g *gatedReadsFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(g.FS, name)
}
func (g *gatedReadsFS) ReadFile(name string) ([]byte, error) {
	if strings.HasSuffix(name, ".wasm") || strings.HasSuffix(name, ".zip") {
		if g.reads.Add(1) == 1 {
			close(g.first)
			<-g.release
		}
	}
	return fs.ReadFile(g.FS, name)
}

func TestPollStableExcludesConcurrentPoll(t *testing.T) {
	h := coreHost(t)
	gate := &gatedReadsFS{
		FS:    fstest.MapFS{"plugins/demo.egop.wasm": &fstest.MapFile{Data: demoModule(t)}},
		first: make(chan struct{}), release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(unblock)
	w := New(h, []string{"plugins"}, Options{FS: gate})
	stableDone := make(chan []Event, 1)
	go func() { stableDone <- w.PollStable(context.Background()) }()
	select {
	case <-gate.first:
	case <-time.After(2 * time.Second):
		t.Fatal("stable poll did not reach its first confirmation read")
	}

	pollDone := make(chan []Event, 1)
	go func() { pollDone <- w.Poll(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // Let the competing poll queue on the watcher mutex.
	unblock()
	var stableEvents, pollEvents []Event
	select {
	case stableEvents = <-stableDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stable poll did not finish")
	}
	select {
	case pollEvents = <-pollDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent poll did not finish after stable polling")
	}
	if hasAction(t, pollEvents, ActionRegister) {
		t.Fatalf("concurrent poll stole the second confirmation round: stable=%v concurrent=%v", stableEvents, pollEvents)
	}
	if !hasAction(t, stableEvents, ActionRegister) || !h.HasPlugin("wasm.demo") {
		t.Fatalf("stable poll must own the registration: %v", stableEvents)
	}
	w.Stop()
	w.Unload(context.Background())
}

type blockedMath struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockedMath) Meta() contract.Meta {
	return contract.Meta{ID: "dummy.math", Name: "Math", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "add"}}}}
}

func (p *blockedMath) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	p.once.Do(func() { close(p.started) })
	<-p.release
	return json.RawMessage(`3`), nil
}

func TestReplaceTransientFailureRetriesSameCandidate(t *testing.T) {
	h := host.New[any](host.Options[any]{ReplaceQuiesce: 40 * time.Millisecond})
	math := &blockedMath{started: make(chan struct{}), release: make(chan struct{})}
	if err := h.Register(math); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("initial register failed")
	}

	callDone := make(chan error, 1)
	callExited := make(chan struct{})
	go func() {
		defer close(callExited)
		_, err := h.Call(context.Background(), "wasm.demo", "call", json.RawMessage(`{}`))
		callDone <- err
	}()
	select {
	case <-math.started:
	case <-time.After(2 * time.Second):
		t.Fatal("old plugin call did not block")
	}
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(math.release) }) }
	t.Cleanup(func() {
		unblock()
		select {
		case <-callExited:
		case <-time.After(2 * time.Second):
			t.Error("blocked call did not finish during cleanup")
		}
		w.Stop()
		w.Unload(context.Background())
	})

	writeZip(t, dir, "demo.egop.zip", demoManifest, paddedModule(t))
	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("first changed poll should only stage the candidate: %v", evs)
	}
	evs := w.Poll(context.Background())
	if !hasAction(t, evs, ActionFailed) {
		t.Fatalf("busy old generation should transiently refuse replacement: %v", evs)
	}
	key := filepath.Join(dir, "demo.egop.zip")
	u := w.units[key]
	if u == nil || u.np == nil {
		t.Fatalf("transient replacement failure must retain its loaded candidate: %+v", u)
	}
	candidate := u.np
	old := u.plugin
	unblock()
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}

	evs = w.Poll(context.Background())
	if !hasAction(t, evs, ActionReplace) {
		t.Fatalf("unchanged candidate should retry after the old call drains: %v", evs)
	}
	if u.plugin != candidate || u.np != nil {
		t.Fatal("retry must install the retained candidate without recompiling it")
	}
	if u.plugin == old {
		t.Fatal("replacement did not change the active instance")
	}
	if _, err := candidate.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("retained candidate is not usable after replacement: %v", err)
	}
}

type transientReplaceHost struct {
	loader.HostFace
	refuse bool
}

func (h *transientReplaceHost) Replace(p contract.Plugin) error {
	if h.refuse {
		return errors.New("transient replacement refusal")
	}
	return h.HostFace.Replace(p)
}

func TestReplacementRollbackDiscardsRetainedCandidate(t *testing.T) {
	h := &transientReplaceHost{HostFace: coreHost(t), refuse: true}
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	key := filepath.Join(dir, "demo.egop.zip")
	u := w.units[key]
	if u == nil || !u.loaded {
		t.Fatal("initial register failed")
	}
	old, oldHash := u.plugin, u.hash
	t.Cleanup(func() {
		w.Stop()
		w.Unload(context.Background())
		_ = old.Close(context.Background())
	})

	writeZip(t, dir, "demo.egop.zip", demoManifest, paddedModule(t))
	evs := settle(t, w, 2)
	if !hasAction(t, evs, ActionFailed) || u.np == nil {
		t.Fatalf("refused replacement should retain a candidate: events=%v unit=%+v", evs, u)
	}
	candidate := u.np
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w.Poll(context.Background())
	if u.np != nil || u.plugin != old || u.hash != oldHash {
		t.Fatal("rollback to the active content must discard the retained candidate")
	}
	if _, err := candidate.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err == nil {
		t.Fatal("candidate retained after rollback must be closed")
	}

	h.refuse = false
	writeZip(t, dir, "demo.egop.zip", demoManifest, paddedModule(t))
	evs = settle(t, w, 3)
	if !hasAction(t, evs, ActionReplace) || u.plugin == old {
		t.Fatalf("candidate content must be usable after a rollback: %v", evs)
	}
}

type metaOnlyPlugin struct{ meta contract.Meta }

func (p *metaOnlyPlugin) Meta() contract.Meta { return p.meta }

type registerTrackingHost struct {
	loader.HostFace
	registered    contract.Plugin
	registrations int
}

func (h *registerTrackingHost) Register(p contract.Plugin) error {
	err := h.HostFace.Register(p)
	if err == nil && p.Meta().ID == "wasm.sec" {
		h.registered = p
		h.registrations++
	}
	return err
}

func TestReidentifyRemoveFailureKeepsOldUnitAuthoritative(t *testing.T) {
	base := coreHost(t)
	h := &registerTrackingHost{HostFace: base}
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	w := New(h, []string{dir}, Options{})
	settle(t, w, 3)
	key := filepath.Join(dir, "demo.egop.zip")
	u := w.units[key]
	if u == nil || !u.loaded {
		t.Fatal("initial register failed")
	}
	old := u.plugin
	oldHash := u.hash
	t.Cleanup(func() {
		w.Stop()
		w.Unload(context.Background())
		_ = old.Close(context.Background())
	})
	dependent := &metaOnlyPlugin{meta: contract.Meta{ID: "dependent", Name: "D", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "wasm.demo", Kind: contract.DepInit}}}}}
	if err := base.Register(dependent); err != nil {
		t.Fatal(err)
	}

	newManifest := `{"id":"wasm.sec","name":"S","version":"2","provides":{"functions":[{"name":"add"}]}}`
	writeZip(t, dir, "demo.egop.zip", newManifest, demoModule(t))
	evs := settle(t, w, 3)
	if !base.HasPlugin("wasm.demo") {
		t.Fatal("old plugin must remain registered when its removal is refused")
	}
	if base.HasPlugin("wasm.sec") {
		t.Fatal("new candidate must be rolled back when the old id cannot be removed")
	}
	if u.id != "wasm.demo" || u.plugin != old || u.hash != oldHash {
		t.Fatalf("old unit must remain authoritative: id=%q plugin=%p old=%p", u.id, u.plugin, old)
	}
	if !hasAction(t, evs, ActionFailed) {
		t.Fatalf("failed re-identification must be reported as failed: %v", evs)
	}
	if h.registered == nil {
		t.Fatal("test did not observe the new candidate registration")
	}
	candidate, ok := h.registered.(contract.FunctionProvider)
	if !ok {
		t.Fatal("new candidate should provide functions")
	}
	if _, err := candidate.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err == nil {
		t.Fatal("rolled-back candidate must be closed")
	}
	if _, err := old.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("old plugin must remain usable: %v", err)
	}
	before := h.registrations
	if extra := settle(t, w, 4); len(extra) != 0 {
		t.Fatalf("unchanged rejected candidate must stay dormant: %v", extra)
	}
	if h.registrations != before {
		t.Fatalf("unchanged rejected candidate registered again: before=%d after=%d", before, h.registrations)
	}
}
