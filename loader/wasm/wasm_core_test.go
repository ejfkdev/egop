// core 侧 WASM loader 自检:夹具 testdata/demo.wat 编译产物 demo.wasm,
// 经 core Host 全链路:加载/清单/函数/跨插件调用/KV 门控/事件推送/工具 Raw。
package wasm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

func readDemo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type pointsRec struct {
	mu     sync.Mutex
	points map[string]bool
}

func (p *pointsRec) EnsurePoint(pt string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.points == nil {
		p.points = map[string]bool{}
	}
	p.points[pt] = true
}

type eventsRec struct {
	mu   sync.Mutex
	subs map[string]func(context.Context, contract.Event)
}

func newEvents() *eventsRec {
	return &eventsRec{subs: map[string]func(context.Context, contract.Event){}}
}

func (e *eventsRec) Subscribe(f *contract.EventFilter, fn func(context.Context, contract.Event)) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 测试假总线:只按精确 Type 键控(忽略通配/来源/标签)。
	typ := ""
	if f != nil {
		typ = f.Type
	}
	e.subs[typ] = fn
	return func() { e.mu.Lock(); delete(e.subs, typ); e.mu.Unlock() }
}

func (e *eventsRec) Dispatch(ctx context.Context, ev contract.Event) {
	e.mu.Lock()
	fn := e.subs[ev.Type]
	e.mu.Unlock()
	if fn != nil {
		fn(ctx, ev)
	}
}

func (e *eventsRec) EnsureTopic(string) {}

// mathPlugin 跨插件调用目标(demo.wat 内 call → dummy.math.add)。
type mathPlugin struct{}

func (mathPlugin) Meta() contract.Meta {
	return contract.Meta{ID: "dummy.math", Name: "M", Version: "1", Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "add"}}}}
}

func (mathPlugin) CallFunc(_ context.Context, _ string, input json.RawMessage) (json.RawMessage, error) {
	var a struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return nil, err
	}
	return json.RawMessage(strconv.Itoa(a.A + a.B)), nil
}

type settingsSource map[string]json.RawMessage

func (s settingsSource) Get(key string) (json.RawMessage, bool) {
	v, ok := s[key]
	return v, ok
}

// memSt 是 wasm 测试用的最小内存持久化(仅需 KV:kv 回程测试只读 found:false)。
type memSt struct{}

func (memSt) File(string) contract.FileStore { return nil }
func (memSt) KV(string) contract.KeyValue    { return &memKVD{} }

type memKVD struct{ m map[string][]byte }

func (k *memKVD) Get(key string) ([]byte, bool) {
	v, ok := k.m[key]
	return v, ok
}
func (k *memKVD) Put(key string, v []byte) {
	if k.m == nil {
		k.m = map[string][]byte{}
	}
	k.m[key] = v
}
func (k *memKVD) Delete(key string) { delete(k.m, key) }
func (k *memKVD) Keys() []string {
	out := make([]string, 0, len(k.m))
	for kk := range k.m {
		out = append(out, kk)
	}
	return out
}

func coreHost(t *testing.T) (*host.Host[any], *eventsRec) {
	t.Helper()
	ev := newEvents()
	pts := &pointsRec{}
	settings := settingsSource{"test.setting": json.RawMessage(`"hello"`)}
	h := host.New[any](host.Options[any]{
		Points:   pts,
		Events:   ev,
		Settings: settings,
		Storage:  memSt{},
		ExecFn:   func(ctx context.Context, cmd string) (string, error) { return "exec:" + cmd, nil },
	})
	if err := h.Register(mathPlugin{}); err != nil {
		t.Fatal(err)
	}
	return h, ev
}

func mustLoad(t *testing.T) *Plugin {
	t.Helper()
	p, err := LoadFS(context.Background(), readDemo(t), "demo.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	return p
}

func TestLoadWasmAndCallFaces(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	if p.Meta().ID != "wasm.demo" || len(p.Meta().Provides.Functions) != 3 {
		t.Fatalf("meta = %+v", p.Meta())
	}
	// add → 静态信封 {"ok":true,"result":42}
	out, err := p.CallFunc(context.Background(), "add", json.RawMessage(`{}`))
	if err != nil || string(out) != "42" {
		t.Fatalf("add = %s, %v", out, err)
	}
	raw, ok := p.ToolRaw("answer")
	if !ok {
		t.Fatal("answer tool missing")
	}
	s, err := raw(context.Background(), json.RawMessage(`{"run_id":"r1"}`), json.RawMessage(`{"q":1}`))
	if err != nil || s != "42" {
		t.Fatalf("tool = %q, %v", s, err)
	}
}

func TestHostIntegrationCallAndEvents(t *testing.T) {
	h, ev := coreHost(t)
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	// flen==4 → 宿主注入 call 转 dummy.math.add({a:1,b:2}),guest 原样回传 → result 3
	out, err := h.Call(context.Background(), "wasm.demo", "call", json.RawMessage(`{}`))
	if err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Fatalf("call = %s, %v", out, err)
	}
	// flen==2 → kv_get("key"):无既有值 → {"ok":true,"result":{"found":false}}
	out, err = h.Call(context.Background(), "wasm.demo", "kv", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("kv: %v", err)
	}
	if !strings.Contains(string(out), "found") {
		t.Fatalf("kv out = %s", out)
	}
	// egop_init 已订阅 {"type":"wasm.test.topic"};Dispatch 后 egop_on_event 把整事件 JSON 拷到 3800
	payload := json.RawMessage(`{"n":1}`)
	evt := contract.Event{Type: "wasm.test.topic", Version: contract.EnvelopeVersion, Payload: payload}
	ev.Dispatch(context.Background(), evt)
	eventJSON, _ := json.Marshal(evt)
	mem := p.mod.Memory()
	b, ok := mem.Read(3800, uint32(len(eventJSON)))
	if !ok || !bytes.Equal(b, eventJSON) {
		t.Fatalf("event buffer = %q ok=%v (want %s)", b, ok, eventJSON)
	}
	if !h.HasPlugin("wasm.demo") {
		t.Fatal("plugin lost")
	}
}

func TestHostHookRoundtrip(t *testing.T) {
	// egop_init 里 guest 调 on_hook("demo.hook") 注册;宿主 TriggerHook 发回
	// egop_on_hook,guest 返回 HookResult 信封(remote/wasm 与 Go 侧 OnHook 同语义)。
	h, _ := coreHost(t)
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}

	results := h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{"x":1}`))
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	r := results[0]
	if !r.Block || r.Reason != "wasm says no" || string(r.Data) != `{"seen":1}` {
		t.Fatalf("hook result = %+v", r)
	}
	if r.Origin == nil || r.Origin.ID != "wasm.demo" || r.Seq != 1 || r.Origin.At == 0 {
		t.Fatalf("framework fields origin=%+v seq=%d", r.Origin, r.Seq)
	}
}

func TestGateDeniedKV(t *testing.T) {
	h, _ := coreHost(t)
	zipBytes := buildZip(t, `{"id":"wasm.gate","name":"G","version":"1","provides":{"capabilities":["plugin.call","event.listen"],"functions":[{"name":"kv"}]}}`, readDemo(t))
	p, err := LoadFS(context.Background(), zipBytes, "gate.egop.zip", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(context.Background(), "wasm.gate", "kv", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("gate kv err = %v", err)
	}
}

func TestCustomSectionManifestWins(t *testing.T) {
	sec := `{"id":"wasm.sec","name":"S","version":"2","provides":{"functions":[{"name":"add"}]}}`
	bin := append(append([]byte{}, readDemo(t)...), customSection(ManifestSection, []byte(sec))...)
	p, err := LoadFS(context.Background(), bin, "sec.egop.wasm", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	if p.Meta().ID != "wasm.sec" {
		t.Fatalf("section not preferred: %s", p.Meta().ID)
	}
}

// ---- 夹具工具 ----

// customSection 构造 wasm 自定义段(id=0):段名 + 载荷。
func customSection(name string, payload []byte) []byte {
	var out []byte
	out = append(out, 0x00)
	content := append(uleb(len(name)), name...)
	content = append(content, payload...)
	out = append(out, uleb(len(content))...)
	return append(out, content...)
}

func uleb(v int) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// buildZip 内联打包 manifest.json + plugin.wasm(.egop.zip 形态)。
func buildZip(t *testing.T, manifest string, module []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(module); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readMeta(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "meta.egop.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// svcPlugin 是 wasm 插件跨插件配置读写测试的目标(可下发配置)。
type svcPlugin struct{ meta contract.Meta }

func (s *svcPlugin) Meta() contract.Meta               { return s.meta }
func (s *svcPlugin) ApplyConfig(json.RawMessage) error { return nil }

// TestHostMetaAndConfigImports 验证 wasm 插件的四个目录/配置注入函数
// (plugins/get_plugin/get_config/set_config)整段 ABI + 能力门控。夹具 meta.egop.wasm
// 在 egop_init 里依次调用它们,把结果信封 (len<<32|ptr) 存到固定内存槽位。
func TestHostMetaAndConfigImports(t *testing.T) {
	h := host.New[any](host.Options[any]{})
	svc := &svcPlugin{meta: contract.Meta{ID: "svc", Name: "Svc", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "key", Readable: true, Writable: true}}}}}
	if err := h.Register(svc); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("svc", json.RawMessage(`{"key":"init"}`)); err != nil {
		t.Fatal(err)
	}

	p, err := LoadFS(context.Background(), readMeta(t), "meta.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	readEnv := func(addr uint32) map[string]json.RawMessage {
		t.Helper()
		slot, ok := p.mod.Memory().Read(addr, 8)
		if !ok {
			t.Fatalf("read slot @%d", addr)
		}
		ptr, n := unpack(binary.LittleEndian.Uint64(slot))
		envBytes, ok := p.mod.Memory().Read(ptr, n)
		if !ok {
			t.Fatalf("read envelope @%d len %d", ptr, n)
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(envBytes, &env); err != nil {
			t.Fatalf("bad envelope: %v (%s)", err, envBytes)
		}
		return env
	}

	// plugins:列表含 svc 与 wasm.meta。
	pluginsEnv := readEnv(5000)
	var pls []contract.Meta
	if err := json.Unmarshal(pluginsEnv["result"], &pls); err != nil {
		t.Fatalf("plugins result: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range pls {
		seen[m.ID] = true
	}
	for _, want := range []string{"svc", "wasm.meta"} {
		if !seen[want] {
			t.Fatalf("plugins missing %q (got %v)", want, pls)
		}
	}

	// get_plugin("svc"):取到元数据。
	gpEnv := readEnv(5008)
	var gp struct {
		Found bool          `json:"found"`
		Meta  contract.Meta `json:"meta"`
	}
	if err := json.Unmarshal(gpEnv["result"], &gp); err != nil || !gp.Found || gp.Meta.ID != "svc" {
		t.Fatalf("get_plugin = %s (%v)", gpEnv["result"], err)
	}

	// get_config("svc","key"):可读字段返回初值。
	gcEnv := readEnv(5016)
	var gc struct {
		Found bool            `json:"found"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(gcEnv["result"], &gc); err != nil || !gc.Found || string(gc.Value) != `"init"` {
		t.Fatalf("get_config = %s (%v)", gcEnv["result"], err)
	}

	// set_config("svc","key"):写成功,宿主读回新值。
	scEnv := readEnv(5024)
	if string(scEnv["ok"]) != "true" {
		t.Fatalf("set_config envelope not ok: %v", scEnv)
	}
	if v, _ := h.GetConfig("svc", "key"); string(v) != `"new"` {
		t.Fatalf("svc.key after wasm set_config = %s", v)
	}
}
