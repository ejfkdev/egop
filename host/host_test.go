// host 泛化宿主的 B1 自检:注册/函数目录/槽位八轴+Needs/配置校验/工具收集/卸载级联。
package host

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

type fakePoints struct {
	mu     sync.Mutex
	points map[string]bool
}

func (f *fakePoints) EnsurePoint(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.points == nil {
		f.points = map[string]bool{}
	}
	f.points[p] = true
}

type demoPlugin struct {
	meta contract.Meta
	tool string
}

func (d *demoPlugin) Meta() contract.Meta { return d.meta }

func (d *demoPlugin) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

func (d *demoPlugin) ApplyConfig(cfg json.RawMessage) error { return nil }

func (d *demoPlugin) ToolSpecs() []contract.FuncSpec {
	return []contract.FuncSpec{{Name: d.tool}}
}

func (d *demoPlugin) Tool(name string) (contract.ToolFunc[struct{}], bool) {
	if name != d.tool {
		return nil, false
	}
	return func(ctx context.Context, tctx *struct{}, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"hit"`), nil
	}, true
}

func TestHostRegisterFuncsAndTools(t *testing.T) {
	pts := &fakePoints{}
	h := New[struct{}](Options[struct{}]{Points: pts})
	p := &demoPlugin{meta: contract.Meta{
		ID: "demo.p", Name: "D", Version: "1",
		Provides: contract.Provides{
			Functions:    []contract.FuncSpec{{Name: "ping"}},
			Capabilities: []string{"tool.provide"},
		},
	}, tool: "probe"}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	out, err := h.Call(context.Background(), "demo.p", "ping", nil)
	if err != nil || string(out) != `"ok"` {
		t.Fatalf("call = %s, %v", out, err)
	}
	tools := h.Tools()
	if len(tools) != 1 || tools[0].Spec.Name != "probe" {
		t.Fatalf("tools = %+v", tools)
	}
	if out, err := tools[0].Run(context.Background(), &struct{}{}, json.RawMessage(`{}`)); err != nil || out != `"hit"` {
		t.Fatalf("tool run = %q, %v", out, err)
	}
}

func TestHostSlotSixAxesAndNeeds(t *testing.T) {
	pts := &fakePoints{}
	slots := map[string]contract.SlotSpec{
		"demo.greeter": {
			ID: "demo.greeter", Doc: "d", Builtin: true,
			Provides: []string{"run.begin", "run.end"},
		},
		"demo.ext": {
			ID: "demo.ext", Doc: "d",
			Provides:  []string{"run.end"},
			Functions: []string{"ping"},
			Needs:     []string{"demo.greeter"},
		},
	}
	h := New[struct{}](Options[struct{}]{Points: pts, SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	// 缺 Provides/Functions → 拒注
	bad := &demoPlugin{meta: contract.Meta{ID: "x", Name: "X", Version: "1", Slot: "demo.ext"}}
	if err := h.Register(bad); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("incomplete claim err = %v", err)
	}
	good := &demoPlugin{meta: contract.Meta{
		ID: "y", Name: "Y", Version: "1",
		Slot: "demo.ext",
		Provides: contract.Provides{
			Points:    []string{"run.end"},
			Functions: []contract.FuncSpec{{Name: "ping"}},
		},
	}}
	if err := h.Register(good); err != nil {
		t.Fatalf("complete claim rejected: %v", err)
	}
	// Provides 已埋点
	if !pts.points["run.end"] {
		t.Fatal("emit point not ensured")
	}
}

func TestHostConfigValidationAndRemoveCascade(t *testing.T) {
	h := New[struct{}](Options[struct{}]{})
	p := &demoPlugin{meta: contract.Meta{
		ID: "cfg.p", Name: "C", Version: "1",
		Provides: contract.Provides{
			Config: []contract.ConfigFieldSpec{
				{Key: "level", Schema: json.RawMessage(`{"type":"integer"}`)},
			},
		},
	}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("cfg.p", json.RawMessage(`{"level":"high"}`)); err == nil || !strings.Contains(err.Error(), "level: expected integer") {
		t.Fatalf("bad config err = %v", err)
	}
	if err := h.SetConfig("cfg.p", json.RawMessage(`{"level":3}`)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	dep := &demoPlugin{meta: contract.Meta{
		ID: "dep.p", Name: "Dep", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "cfg.p", Kind: contract.DepInit}}},
	}}
	if err := h.Register(dep); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Remove("cfg.p", false); err == nil {
		t.Fatal("non-cascade remove of depended plugin must refuse")
	}
	removed, err := h.Remove("cfg.p", true)
	if err != nil || len(removed) != 2 {
		t.Fatalf("cascade = %v, %v", removed, err)
	}
	if h.HasPlugin("dep.p") {
		t.Fatal("cascade did not unload dependent")
	}
}

func TestHostSlotEventsAxis(t *testing.T) {
	// SlotSpec.Events 是第八轴:槽位可要求实现者发布指定事件主题。
	slots := map[string]contract.SlotSpec{
		"demo.events": {ID: "demo.events", Doc: "d", Events: []string{"run.started"}},
	}
	h := New[struct{}](Options[struct{}]{SlotLookup: func(id string) (contract.SlotSpec, bool) {
		s, ok := slots[id]
		return s, ok
	}})
	bad := &demoPlugin{meta: contract.Meta{ID: "x", Name: "X", Version: "1", Slot: "demo.events"}}
	if err := h.Register(bad); err == nil || !strings.Contains(err.Error(), "event topic") {
		t.Fatalf("missing event topic err = %v", err)
	}
	good := &demoPlugin{meta: contract.Meta{
		ID: "y", Name: "Y", Version: "1", Slot: "demo.events",
		Provides: contract.Provides{Events: []contract.EventTopicSpec{{ID: "run.started"}}},
	}}
	if err := h.Register(good); err != nil {
		t.Fatalf("complete claim rejected: %v", err)
	}
}

func TestPluginsRegistrationOrder(t *testing.T) {
	// meta 是 map,直接遍历顺序不定;Plugins() 应回按注册序(seq 即注册序)。
	h := New[any](Options[any]{})
	want := []string{"p.1", "p.2", "p.3"}
	for _, id := range want {
		if err := h.Register(&demoPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1"}}); err != nil {
			t.Fatal(err)
		}
	}
	got := h.Plugins()
	if len(got) != len(want) {
		t.Fatalf("plugins = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("plugins[%d] = %q, want %q (registration order not preserved)", i, got[i].ID, want[i])
		}
	}
}

func TestPersistListIsolation(t *testing.T) {
	// List 只应返回本插件自己的文件(隔离由注入的 Storage 后端保证)。
	h := New[any](Options[any]{Storage: newMemStorage()})
	for _, id := range []string{"p.a", "p.b"} {
		if err := h.Register(&demoPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1",
			Provides: contract.Provides{Capabilities: []string{contract.CapPersist}}}}); err != nil {
			t.Fatal(err)
		}
	}
	sa, ok := h.SurfaceFor("p.a")
	if !ok {
		t.Fatal("surface p.a missing")
	}
	sb, _ := h.SurfaceFor("p.b")
	fa, ok := sa.Persist()
	if !ok {
		t.Fatal("p.a persist not available")
	}
	fb, _ := sb.Persist()
	if err := fa.Write("a.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := fb.Write("b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	la, _ := fa.List()
	lb, _ := fb.List()
	if len(la) != 1 || la[0] != "a.txt" {
		t.Fatalf("p.a List = %v (want [a.txt])", la)
	}
	if len(lb) != 1 || lb[0] != "b.txt" {
		t.Fatalf("p.b List = %v (want [b.txt])", lb)
	}
}

func TestConfigFieldAccessControl(t *testing.T) {
	// 跨插件配置访问:egop 始终可读写;其它插件须声明 config.read/config.write 能力,
	// 且目标字段标记 Readable/Writable,两层都满足才放行(默认全拒)。
	h := New[any](Options[any]{})

	target := &demoPlugin{meta: contract.Meta{
		ID: "cfg.target", Name: "T", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{
			{Key: "api_key", Writable: true, Readable: false}, // 只写(如 apikey:egop 可读,别的插件只能写)
			{Key: "public", Writable: false, Readable: true},  // 只读
			{Key: "rw", Writable: true, Readable: true},       // 读写
			{Key: "locked", Writable: false, Readable: false}, // 全拒
		}},
	}}
	if err := h.Register(target); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("cfg.target", json.RawMessage(`{"api_key":"sk-1","public":"hello","rw":"x","locked":"y"}`)); err != nil {
		t.Fatal(err)
	}

	reader := &demoPlugin{meta: contract.Meta{ID: "cfg.reader", Name: "R", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapConfigRead}}}}
	writer := &demoPlugin{meta: contract.Meta{ID: "cfg.writer", Name: "W", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapConfigWrite}}}}
	bystander := &demoPlugin{meta: contract.Meta{ID: "cfg.none", Name: "N", Version: "1"}}
	for _, p := range []contract.Plugin{reader, writer, bystander} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := h.SurfaceFor("cfg.reader")
	ws, _ := h.SurfaceFor("cfg.writer")
	ns, _ := h.SurfaceFor("cfg.none")

	// 读:readable=true 且 caller 有 config.read 才可见。
	if v, ok := rs.GetConfig("cfg.target", "public"); !ok || string(v) != `"hello"` {
		t.Fatalf("reader public = %s, %v", v, ok)
	}
	if _, ok := rs.GetConfig("cfg.target", "api_key"); ok {
		t.Fatal("api_key (readable=false) should be unreadable")
	}
	if _, ok := rs.GetConfig("cfg.target", "locked"); ok {
		t.Fatal("locked should be unreadable")
	}
	if _, ok := ws.GetConfig("cfg.target", "public"); ok {
		t.Fatal("writer lacks config.read, should not read")
	}
	if _, ok := ns.GetConfig("cfg.target", "public"); ok {
		t.Fatal("bystander lacks config.read, should not read")
	}

	// 写:只有 writable=true 且 caller 有 config.write 才写。
	if err := ws.SetConfig("cfg.target", "api_key", json.RawMessage(`"sk-2"`)); err != nil {
		t.Fatalf("writer set api_key: %v", err)
	}
	if v, _ := h.GetConfig("cfg.target", "api_key"); string(v) != `"sk-2"` {
		t.Fatalf("api_key after write = %s", v)
	}
	if err := ws.SetConfig("cfg.target", "public", json.RawMessage(`"nope"`)); err == nil {
		t.Fatal("public (writable=false) should reject writes")
	}
	if err := ns.SetConfig("cfg.target", "rw", json.RawMessage(`"z"`)); err == nil {
		t.Fatal("bystander lacks config.write, should not write")
	}
	// 只写字段不能被写者自己读回(无 config.read)。
	if _, ok := ws.GetConfig("cfg.target", "api_key"); ok {
		t.Fatal("write-only api_key should not be readable by the writer")
	}
}

func TestRegisterLazyDefersUntilDeps(t *testing.T) {
	// B 依赖 A(A 未注册):RegisterLazy 入待补载;A 注册后 B 自动补载。无依赖则立即注册。
	h := New[any](Options[any]{})
	b := &demoPlugin{meta: contract.Meta{ID: "b", Name: "B", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "a", Kind: contract.DepInit}}}}}
	st, err := h.RegisterLazy(b)
	if err != nil {
		t.Fatal(err)
	}
	if st != StatusPending {
		t.Fatalf("b status = %v (want pending)", st)
	}
	if h.HasPlugin("b") {
		t.Fatal("b should not be registered yet")
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "a", Name: "A", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if !h.HasPlugin("b") {
		t.Fatal("b should be auto-registered after a appears")
	}

	// 无依赖的插件立即注册。
	c := &demoPlugin{meta: contract.Meta{ID: "c", Name: "C", Version: "1"}}
	st, err = h.RegisterLazy(c)
	if err != nil || st != StatusRegistered {
		t.Fatalf("c should register immediately: status=%v err=%v", st, err)
	}
	if !h.HasPlugin("c") {
		t.Fatal("c should be registered")
	}
}

func TestRegisterLazyTransitive(t *testing.T) {
	// c→b→a 链:先懒注册 c、b,a 到位后整链自动补载。
	h := New[any](Options[any]{})
	c := &demoPlugin{meta: contract.Meta{ID: "c", Name: "C", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "b", Kind: contract.DepInit}}}}}
	b := &demoPlugin{meta: contract.Meta{ID: "b", Name: "B", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "a", Kind: contract.DepInit}}}}}
	if _, err := h.RegisterLazy(c); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RegisterLazy(b); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "a", Name: "A", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !h.HasPlugin(id) {
			t.Fatalf("transitive lazy load failed: %s not registered (got %v)", id, h.Plugins())
		}
	}
}

// lazyDisposer 是带 Disposer 的惰性插件,验证 Close 时待补载插件也被清退。
type lazyDisposer struct {
	meta   contract.Meta
	closed bool
}

func (d *lazyDisposer) Meta() contract.Meta         { return d.meta }
func (d *lazyDisposer) Close(context.Context) error { d.closed = true; return nil }

func TestCloseDisposesPendingLazy(t *testing.T) {
	h := New[any](Options[any]{})
	d := &lazyDisposer{meta: contract.Meta{ID: "lazy.d", Name: "L", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "never.exists", Kind: contract.DepInit}}}}}
	st, err := h.RegisterLazy(d)
	if err != nil || st != StatusPending {
		t.Fatalf("RegisterLazy = status=%v err=%v (want pending, no err)", st, err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.closed {
		t.Fatal("pending lazy plugin should be disposed on Close")
	}
}

func TestRegisterLazyReplacesPending(t *testing.T) {
	// 重复懒登记同 id:更新(非错误)仍 pending;依赖到位后自动补载。
	h := New[any](Options[any]{})
	mk := func(ver string) contract.Plugin {
		return &demoPlugin{meta: contract.Meta{ID: "lazy.x", Name: "X", Version: ver,
			Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "never", Kind: contract.DepInit}}}}}
	}
	if st, err := h.RegisterLazy(mk("1")); err != nil || st != StatusPending {
		t.Fatalf("first lazy = status=%v err=%v", st, err)
	}
	if st, err := h.RegisterLazy(mk("2")); err != nil || st != StatusPending {
		t.Fatalf("re-lazy = status=%v err=%v (want update, still pending)", st, err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "never", Name: "N", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if !h.HasPlugin("lazy.x") {
		t.Fatal("lazy.x should auto-register after its dep lands")
	}
}

func TestRegisterManyDefersPendingDep(t *testing.T) {
	// 批量里依赖 pending 插件的条目 → 转为待补载(Pending)而非失败;依赖链后到则整链补载。
	h := New[any](Options[any]{})
	if _, err := h.RegisterLazy(&demoPlugin{meta: contract.Meta{ID: "dep", Name: "D", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "never", Kind: contract.DepInit}}}}}); err != nil {
		t.Fatal(err)
	}
	rep := h.RegisterMany([]contract.Plugin{
		&demoPlugin{meta: contract.Meta{ID: "consumer", Name: "C", Version: "1",
			Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "dep", Kind: contract.DepInit}}}}},
	})
	if len(rep.Failed) != 0 {
		t.Fatalf("failed = %v (want none)", rep.Failed)
	}
	if len(rep.Pending) != 1 || rep.Pending[0] != "consumer" {
		t.Fatalf("pending = %v (want [consumer])", rep.Pending)
	}
	if h.HasPlugin("consumer") {
		t.Fatal("consumer should be pending, not registered")
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "never", Name: "N", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if !h.HasPlugin("dep") || !h.HasPlugin("consumer") {
		t.Fatalf("chain load failed: dep=%v consumer=%v", h.HasPlugin("dep"), h.HasPlugin("consumer"))
	}
}

func TestPluginMetaGating(t *testing.T) {
	// 插件目录/元数据读取需声明 plugin.meta;未声明者完全不可见。
	h := New[any](Options[any]{})
	for _, id := range []string{"p.a", "p.b"} {
		if err := h.Register(&demoPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1"}}); err != nil {
			t.Fatal(err)
		}
	}
	obs := &demoPlugin{meta: contract.Meta{ID: "obs", Name: "O", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapPluginMeta}}}}
	blind := &demoPlugin{meta: contract.Meta{ID: "blind", Name: "B", Version: "1"}}
	for _, p := range []contract.Plugin{obs, blind} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	os, _ := h.SurfaceFor("obs")
	bs, _ := h.SurfaceFor("blind")

	if n := len(os.Plugins()); n != 4 {
		t.Fatalf("observer sees %d plugins (want 4)", n)
	}
	if m, ok := os.GetPlugin("p.a"); !ok || m.ID != "p.a" {
		t.Fatalf("observer GetPlugin(p.a) = %+v, %v", m, ok)
	}
	if _, ok := os.GetPlugin("p.b"); !ok {
		t.Fatal("observer GetPlugin(p.b) should be found")
	}
	// 未声明 plugin.meta 的插件全不可见。
	if len(bs.Plugins()) != 0 {
		t.Fatal("blind should see no plugins")
	}
	if _, ok := bs.GetPlugin("p.a"); ok {
		t.Fatal("blind GetPlugin(p.a) should be false")
	}
	if _, ok := bs.GetPlugin("p.a"); ok {
		t.Fatal("blind GetPlugin should be false")
	}
}

// ---- 持久化注入后端(观察用内存实现) ----

type memStorage struct {
	mu    sync.Mutex
	files map[string]map[string][]byte
	kv    map[string]map[string][]byte
}

func newMemStorage() *memStorage {
	return &memStorage{files: map[string]map[string][]byte{}, kv: map[string]map[string][]byte{}}
}
func (s *memStorage) File(pluginID string) contract.FileStore { return &memFile{s: s, id: pluginID} }
func (s *memStorage) KV(pluginID string) contract.KeyValue    { return &memKV{s: s, id: pluginID} }

type memFile struct {
	s  *memStorage
	id string
}

func (f *memFile) Read(name string) ([]byte, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if b, ok := f.s.files[f.id][name]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (f *memFile) Write(name string, data []byte) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if f.s.files[f.id] == nil {
		f.s.files[f.id] = map[string][]byte{}
	}
	f.s.files[f.id][name] = data
	return nil
}
func (f *memFile) List() ([]string, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	out := make([]string, 0, len(f.s.files[f.id]))
	for n := range f.s.files[f.id] {
		out = append(out, n)
	}
	return out, nil
}

type memKV struct {
	s  *memStorage
	id string
}

func (k *memKV) Get(key string) ([]byte, bool) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	v, ok := k.s.kv[k.id][key]
	return v, ok
}
func (k *memKV) Put(key string, v []byte) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if k.s.kv[k.id] == nil {
		k.s.kv[k.id] = map[string][]byte{}
	}
	k.s.kv[k.id][key] = v
}
func (k *memKV) Delete(key string) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	delete(k.s.kv[k.id], key)
}
func (k *memKV) Keys() []string {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	out := make([]string, 0, len(k.s.kv[k.id]))
	for kk := range k.s.kv[k.id] {
		out = append(out, kk)
	}
	return out
}

func TestInjectedStorage(t *testing.T) {
	// 持久化外部注入:不设 StorageRoot 也能 Persist/KV。
	h := New[any](Options[any]{Storage: newMemStorage()})
	p := &demoPlugin{meta: contract.Meta{ID: "p.io", Name: "IO", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapPersist, contract.CapKV}}}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	s, _ := h.SurfaceFor("p.io")

	fs, ok := s.Persist()
	if !ok {
		t.Fatal("persist not available with injected storage")
	}
	if err := fs.Write("f.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if b, err := fs.Read("f.txt"); err != nil || string(b) != "hi" {
		t.Fatalf("read = %q, %v", b, err)
	}

	kv, ok := s.KV()
	if !ok {
		t.Fatal("kv not available with injected storage")
	}
	kv.Put("k", []byte("v"))
	if v, _ := kv.Get("k"); string(v) != "v" {
		t.Fatalf("kv get = %q", v)
	}
}

func TestDependencyMinVersionEnforced(t *testing.T) {
	// Dependency.MinVersion 之前只声明不校验;现在 DepInit 依赖按版本机器把关。
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "dep.base", Name: "B", Version: "1.5.0"}}); err != nil {
		t.Fatal(err)
	}
	needs := &demoPlugin{meta: contract.Meta{ID: "dep.needs", Name: "N", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "dep.base", Kind: contract.DepInit, MinVersion: "2.0.0"}}}}}
	if err := h.Register(needs); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("min-version mismatch err = %v", err)
	}
	// 升级后满足 → 通过。
	if err := h.Replace(&demoPlugin{meta: contract.Meta{ID: "dep.base", Name: "B", Version: "2.1.0"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(needs); err != nil {
		t.Fatalf("register after upgrade: %v", err)
	}
}

func TestPublishEventCarriesSource(t *testing.T) {
	// 事件应携带发布方插件 id(Event.Source);宿主级事件 source 为空。
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "pub", Name: "P", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapEmitsEvents}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "listener", Name: "L", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapListensEvents}}}}); err != nil {
		t.Fatal(err)
	}
	ls, _ := h.SurfaceFor("listener")
	var origin *contract.Origin
	var ver int
	ls.SubscribeEvent("topic.src", func(_ context.Context, _ string, e contract.Event) {
		origin = e.Source
		ver = e.Version
	})
	ps, _ := h.SurfaceFor("pub")
	ps.PublishEvent(context.Background(), "topic.src", json.RawMessage(`{"x":1}`))
	if origin == nil || origin.ID != "pub" || origin.Version != "1" ||
		origin.Kind != contract.OriginEvent || origin.Point != "topic.src" || origin.At == 0 {
		t.Fatalf("event origin = %+v (want id=pub version=1 kind=event point=topic.src at!=0)", origin)
	}
	if ver != contract.EnvelopeVersion {
		t.Fatalf("event version = %d, want %d", ver, contract.EnvelopeVersion)
	}
}

// callerProbe 的 CallFunc 回显调用者 Origin,验证跨插件调用的上下文注入。
type callerProbe struct{ meta contract.Meta }

func (c *callerProbe) Meta() contract.Meta { return c.meta }

func (c *callerProbe) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	o := contract.OriginFrom(ctx)
	id, kind, ver, point := "", "", "", ""
	if o != nil {
		id, kind, ver, point = o.ID, string(o.Kind), o.Version, o.Point
	}
	return json.Marshal(map[string]string{"id": id, "kind": kind, "version": ver, "point": point})
}

func TestCrossPluginCallCarriesCaller(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(&callerProbe{meta: contract.Meta{ID: "callee", Name: "C", Version: "9",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "who"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "caller", Name: "A", Version: "2",
		Provides: contract.Provides{Capabilities: []string{contract.CapCallPlugins}}}}); err != nil {
		t.Fatal(err)
	}

	// 插件发起的跨插件调用 → 被调方能看到调用者 Origin。
	cs, _ := h.SurfaceFor("caller")
	out, err := cs.Call(context.Background(), "callee", "who", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "caller" || got["kind"] != "call" || got["version"] != "2" || got["point"] != "who" {
		t.Fatalf("callee saw caller = %v (want id=caller kind=call version=2 point=who)", got)
	}

	// 宿主直接调用 → 框架来源(Origin{Kind:host}),而非"无调用者"。
	out2, err := h.Call(context.Background(), "callee", "who", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]string
	_ = json.Unmarshal(out2, &got2)
	if got2["id"] != "" || got2["kind"] != "host" || got2["point"] != "who" {
		t.Fatalf("host-level call origin = %v (want id= kind=host point=who)", got2)
	}
}

func TestConfigUpdatedEventSource(t *testing.T) {
	// 宿主级事件(plugin.config.updated)应带 Origin{Kind: host}。
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "cfg", Name: "C", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "k"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "lsn", Name: "L", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapListensEvents}}}}); err != nil {
		t.Fatal(err)
	}
	var origin *contract.Origin
	ls, _ := h.SurfaceFor("lsn")
	ls.SubscribeEvent(contract.EventConfigUpdated, func(_ context.Context, _ string, e contract.Event) {
		origin = e.Source
	})
	if err := h.SetConfig("cfg", json.RawMessage(`{"k":"v"}`)); err != nil {
		t.Fatal(err)
	}
	if origin == nil || origin.Kind != contract.OriginHost || origin.At == 0 {
		t.Fatalf("config.updated source = %+v (want kind=host at!=0)", origin)
	}
}

type selfReplacing struct {
	meta contract.Meta
	h    *Host[any]
}

func (s *selfReplacing) Meta() contract.Meta { return s.meta }
func (s *selfReplacing) ApplyConfig(json.RawMessage) error {
	return s.h.Replace(&noopConf{meta: s.meta})
}

type noopConf struct{ meta contract.Meta }

func (n *noopConf) Meta() contract.Meta               { return n.meta }
func (n *noopConf) ApplyConfig(json.RawMessage) error { return nil }

func TestSetConfigDetectsConcurrentReplace(t *testing.T) {
	// ApplyConfig 里自我替换 → SetConfig 的替换重检应检测到实例已变并报错。
	h := New[any](Options[any]{})
	p := &selfReplacing{meta: contract.Meta{ID: "cfg.repl", Name: "R", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "k"}}}}, h: h}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("cfg.repl", json.RawMessage(`{"k":"v"}`)); err == nil {
		t.Fatal("SetConfig should detect the target was replaced during apply")
	}
}

// fakeNet 是最小出站网络后端(内存实现),验证 net.access 能力门控 + 请求/消息流面。
type fakeNet struct{}

func (fakeNet) Request(ctx context.Context, req contract.Request) (*contract.Response, error) {
	return &contract.Response{
		Status:   200,
		Headers:  map[string]string{"content-type": "text/event-stream"},
		Trailers: map[string]string{"grpc-status": "0"},
		Body:     strings.NewReader("data: hello\n\n"),
	}, nil
}
func (fakeNet) DialStream(ctx context.Context, url string, headers map[string]string) (contract.Stream, error) {
	return &fakeWS{}, nil
}

type fakeWS struct{}

func (fakeWS) Send([]byte) error        { return nil }
func (fakeWS) Recv() ([]byte, error)    { return []byte("ws-msg"), nil }
func (fakeWS) Context() context.Context { return context.Background() }

func TestNetCapability(t *testing.T) {
	h := New[any](Options[any]{Net: fakeNet{}})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "blind", Name: "B", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "netter", Name: "N", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapNet}}}}); err != nil {
		t.Fatal(err)
	}

	bs, _ := h.SurfaceFor("blind")
	if _, ok := bs.Net(); ok {
		t.Fatal("blind (no net.access) should not get Net")
	}
	ns, _ := h.SurfaceFor("netter")
	net, ok := ns.Net()
	if !ok {
		t.Fatal("netter (net.access) should get Net")
	}
	resp, err := net.Request(context.Background(), contract.Request{Method: "GET", URL: "https://x/sse"})
	if err != nil || resp.Status != 200 {
		t.Fatalf("request = %+v, %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "data: hello\n\n" {
		t.Fatalf("body = %q", b)
	}
	if resp.Trailers["grpc-status"] != "0" {
		t.Fatalf("trailers = %v", resp.Trailers)
	}
	ws, err := net.DialStream(context.Background(), "wss://x", nil)
	if err != nil || ws == nil {
		t.Fatalf("stream = %v, %v", ws, err)
	}
	if msg, _ := ws.Recv(); string(msg) != "ws-msg" {
		t.Fatalf("ws msg = %q", msg)
	}
}

// TestNetSchemeGuard 验证出站网络协议门:目标必须是网络协议,file:// 等本地/特殊
// scheme 一律拒绝;Options.NetSchemes 可补充自定义网络协议 scheme。
func TestNetSchemeGuard(t *testing.T) {
	base := Options[any]{Net: fakeNet{}, NetSchemes: []string{"webtransport"}}
	h := New[any](base)
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "netter", Name: "N", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapNet}}}}); err != nil {
		t.Fatal(err)
	}
	ns, _ := h.SurfaceFor("netter")
	net, ok := ns.Net()
	if !ok {
		t.Fatal("netter should get Net")
	}
	ctx := context.Background()

	// 允许:标准网络协议 + 补充方案。
	for _, u := range []string{
		"https://api.example.com/v1", "http://127.0.0.1:8080/x", "HTTPS://example.com",
	} {
		if _, err := net.Request(ctx, contract.Request{Method: "GET", URL: u}); err != nil {
			t.Fatalf("Request(%q) should pass, got %v", u, err)
		}
	}
	for _, u := range []string{"ws://x", "wss://x", "webtransport://x"} {
		if _, err := net.DialStream(ctx, u, nil); err != nil {
			t.Fatalf("DialStream(%q) should pass, got %v", u, err)
		}
	}

	// 拒绝:本地文件 / 无协议 / 非网络 scheme。
	for _, u := range []string{
		"file:///etc/passwd",
		"file:///etc/hostname",
		"data:text/plain,hi",
		"javascript:alert(1)",
		"/etc/passwd",
		"etc/passwd",
		"//example.com/x",
		"",
	} {
		if _, err := net.Request(ctx, contract.Request{Method: "GET", URL: u}); err == nil {
			t.Fatalf("Request(%q) should be rejected", u)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "ftp://x", "unix:///tmp/sock"} {
		if _, err := net.DialStream(ctx, u, nil); err == nil {
			t.Fatalf("DialStream(%q) should be rejected", u)
		}
	}
}
