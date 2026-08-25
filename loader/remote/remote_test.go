// 远程插件通道的传输无关 e2e(真实 net.Pipe 双向字节流,无 gRPC/HTTP/TCP 绑定):
// 覆盖入站(插件主动连框架)与出站(框架主动连插件)两个方向、握手、函数调用、
// 工具、配置、HostCall 回程、事件订阅推送、hook 注册触发、断连自动卸载、
// token 拒斥、WantID 校验、请求超时 pending 清理。
package remote

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// evBus 是 core host 的 Events 消费口内存实现。
type evBus struct {
	mu   sync.Mutex
	subs map[string]func(context.Context, contract.Event)
}

func newEvBus() *evBus { return &evBus{subs: map[string]func(context.Context, contract.Event){}} }

func (b *evBus) Subscribe(f *contract.EventFilter, fn func(context.Context, contract.Event)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 测试假总线:只按精确 Type 键控(忽略通配/来源/标签)。
	typ := ""
	if f != nil {
		typ = f.Type
	}
	b.subs[typ] = fn
	return func() { b.mu.Lock(); delete(b.subs, typ); b.mu.Unlock() }
}

func (b *evBus) Dispatch(ctx context.Context, e contract.Event) {
	b.mu.Lock()
	fn := b.subs[e.Type]
	b.mu.Unlock()
	if fn != nil {
		fn(ctx, e)
	}
}

func (b *evBus) EnsureTopic(string) {}

type remoteSettings map[string]json.RawMessage

func (s remoteSettings) Get(key string) (json.RawMessage, bool) {
	v, ok := s[key]
	return v, ok
}

func remoteTestHost(t *testing.T) (*host.Host[any], *evBus) {
	t.Helper()
	bus := newEvBus()
	settings := remoteSettings{"test.setting": json.RawMessage(`"hello"`)}
	h := host.New[any](host.Options[any]{
		Points:   nil,
		Events:   bus,
		Settings: settings,
		ExecFn:   func(ctx context.Context, cmd string) (string, error) { return "exec:" + cmd, nil },
	})
	return h, bus
}

// inboundPipe 起框架入站(net.Pipe),返回插件侧 Stream。框架侧 ServeStream 跑在
// goroutine;token 非空时做帧级口令校验。
func inboundPipe(t *testing.T, rh RemoteHost, token string) Stream {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	fwStream := BindStream(context.Background(), a)
	go func() { _ = ServeStream(context.Background(), rh, fwStream, token, nil) }()
	return BindStream(context.Background(), b)
}

// outboundRawPlugin 起"第三方插件"(直接操纵原始 JSON Frame,不引用 Session/Host),
// 证明交换格式就是公开的 JSON 帧,任何语言/实现都能讲;返回框架侧 Stream。
func outboundRawPlugin(t *testing.T, manifest []byte) Stream {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	plStream := BindStream(context.Background(), b)
	fwStream := BindStream(context.Background(), a)
	go func() {
		f, err := recvFrame(plStream)
		if err != nil {
			return
		}
		if f.Kind != KindRegister {
			_ = streamReplyErr(plStream, f.Id, "remote: first frame must be Register")
			return
		}
		if err := sendFrame(plStream, &Frame{Id: f.Id, Reply: true, Kind: KindRegister, Manifest: manifest}); err != nil {
			return
		}
		for {
			f, err := recvFrame(plStream)
			if err != nil {
				return
			}
			switch f.Kind {
			case KindCallFunc:
				_ = sendFrame(plStream, &Frame{Id: f.Id, Reply: true, Payload: okEnvelope(f.Input)})
			case KindTool:
				_ = sendFrame(plStream, &Frame{Id: f.Id, Reply: true, Payload: okEnvelope(json.RawMessage(`"tool-out"`))})
			case KindApplyConfig:
				_ = sendFrame(plStream, &Frame{Id: f.Id, Reply: true, Payload: okEnvelope(json.RawMessage("null"))})
			case KindPing:
				_ = sendFrame(plStream, &Frame{Id: f.Id, Reply: true, Payload: okEnvelope(json.RawMessage("null"))})
			}
		}
	}()
	return fwStream
}

const remoteDemoManifest = `{"id":"remote.demo","name":"Remote Demo","version":"1.0.0","provides":{"capabilities":["plugin.call","event.emit","event.listen","storage.kv"],"functions":[{"name":"echo"}]},"tools":[{"name":"answer"}]}`

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func loadManifest(t *testing.T, s string) contract.Manifest {
	t.Helper()
	var mf contract.Manifest
	if err := json.Unmarshal([]byte(s), &mf); err != nil {
		t.Fatal(err)
	}
	return mf
}

// ---------- 入站模式:插件主动连接框架 ----------

func TestInboundRegisterCallAndHostCall(t *testing.T) {
	h, _ := remoteTestHost(t)
	mf := loadManifest(t, remoteDemoManifest)

	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil // echo
		},
	}
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, ops)
	if err != nil {
		t.Fatalf("AttachStream: %v", err)
	}
	defer sess.Close()

	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })

	// 框架 → 插件:函数调用
	out, err := h.Call(context.Background(), "remote.demo", "echo", json.RawMessage(`{"x":[1,2]}`))
	if err != nil || string(out) != `{"x":[1,2]}` {
		t.Fatalf("Call(echo) = %s, %v", out, err)
	}

	// 插件 → 框架:HostCall get_setting(框架侧能力面回程,门控)
	setJSON := json.RawMessage(`{"key":"test.setting"}`)
	setOut, err := sess.HostCall(context.Background(), OpGetSetting, setJSON)
	if err != nil {
		t.Fatalf("HostCall(get_setting): %v", err)
	}
	var setRes map[string]any
	if err := json.Unmarshal(setOut, &setRes); err != nil || setRes["found"] != true || setRes["value"] != "hello" {
		t.Fatalf("get_setting = %s (%v)", setOut, err)
	}

	// 插件 → 框架:未声明能力被拒(echo 清单无 exec.cmd)
	if _, err := sess.HostCall(context.Background(), OpExec, json.RawMessage(`{"cmd":"id"}`)); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("OpExec gate err = %v", err)
	}
}

// ---------- 入站:订阅推送 + 断连卸载 + 重连 + token ----------

func TestInboundSubscribeDisconnectReattach(t *testing.T) {
	h, bus := remoteTestHost(t)
	mf := loadManifest(t, remoteDemoManifest)

	pushed := make(chan struct{}, 1)
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		},
		PushEvent: func(_ context.Context, _ string, e contract.Event) {
			if e.Type == "topic.e2e" && string(e.Payload) == `{"n":7}` {
				select {
				case pushed <- struct{}{}:
				default:
				}
			}
		},
	}
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, ops)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })

	if err := sess.Subscribe(context.Background(), &contract.EventFilter{Type: "topic.e2e"}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	bus.Dispatch(context.Background(), contract.Event{Type: "topic.e2e", Version: contract.EnvelopeVersion, Payload: json.RawMessage(`{"n":7}`)})
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("event push not delivered to plugin")
	}

	// 断连 → 框架自动卸载
	sess.Close()
	waitFor(t, 2*time.Second, func() bool { return !h.HasPlugin("remote.demo") })

	// 重连可重复注册
	sess2, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, ops)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer sess2.Close()
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })
}

func TestRemoteEventFilterAndLabels(t *testing.T) {
	// 事件过滤经远程通道统一:插件侧按过滤条件订阅,框架侧用真 MemEvents 匹配,
	// 命中的完整事件(含 Labels/Source)推到插件。
	bus := host.NewMemEvents()
	h := host.New[any](host.Options[any]{Events: bus})
	mf := loadManifest(t, remoteDemoManifest)

	received := make(chan contract.Event, 1)
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
		PushEvent: func(_ context.Context, _ string, e contract.Event) {
			select {
			case received <- e:
			default:
			}
		},
	}
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })

	if err := sess.Subscribe(context.Background(), &contract.EventFilter{
		Type:   "topic.*",
		Labels: map[string]string{"sev": "high"},
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// 命中:通配 + 标签相等 → 推送完整事件(含 Labels)。
	bus.Dispatch(context.Background(), contract.Event{Type: "topic.message", Version: contract.EnvelopeVersion, Labels: map[string]string{"sev": "high"}, Payload: json.RawMessage(`{"n":1}`)})
	select {
	case e := <-received:
		if e.Type != "topic.message" || e.Version != contract.EnvelopeVersion || e.Labels["sev"] != "high" || string(e.Payload) != `{"n":1}` {
			t.Fatalf("received = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching event not delivered")
	}

	// 未命中:sev=low 不满足标签过滤 → 不推送。
	bus.Dispatch(context.Background(), contract.Event{Type: "topic.message", Version: contract.EnvelopeVersion, Labels: map[string]string{"sev": "low"}, Payload: json.RawMessage(`{"n":2}`)})
	select {
	case e := <-received:
		t.Fatalf("non-matching event should not be delivered: %+v", e)
	case <-time.After(200 * time.Millisecond):
		// ok
	}
}

func TestInboundTokenRejected(t *testing.T) {
	h, _ := remoteTestHost(t)
	plStream := inboundPipe(t, h, "secret")

	// 手工发一帧错误口令的 Register,框架侧应回 token rejected 且不入册。
	if err := sendFrame(plStream, &Frame{Kind: KindRegister, Manifest: json.RawMessage(remoteDemoManifest), Token: "wrong"}); err != nil {
		t.Fatal(err)
	}
	rep, err := recvFrame(plStream)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Error == "" || !strings.Contains(rep.Error, "token rejected") {
		t.Fatalf("wrong token reply error = %q", rep.Error)
	}
	waitFor(t, time.Second, func() bool { return !h.HasPlugin("remote.demo") })
}

func TestAttachStreamToken(t *testing.T) {
	h, _ := remoteTestHost(t)
	mf := loadManifest(t, remoteDemoManifest)

	// 正确口令:WithToken 把 token 送进 Register 帧,与框架 ServeStream("secret") 对等校验通过。
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, "secret"), mf, nil, WithToken("secret"))
	if err != nil {
		t.Fatalf("AttachStream with correct token: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })

	// 错误口令:框架侧回 token rejected,插件不入册(会话被 AttachStream 自行关闭)。
	h2, _ := remoteTestHost(t)
	_, err = AttachStream(context.Background(), inboundPipe(t, h2, "secret"), mf, nil, WithToken("wrong"))
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("wrong token err = %v", err)
	}
	waitFor(t, time.Second, func() bool { return !h2.HasPlugin("remote.demo") })

	sess.Close()
}

// ---------- 出站模式:框架主动连接插件 ----------

const fakeManifest = `{"id":"fake.demo","name":"Fake","version":"1.0.0","provides":{"capabilities":["tool.provide"],"functions":[{"name":"echo"}]},"tools":[{"name":"t1"}]}`

func TestOutboundDialAndCall(t *testing.T) {
	h, _ := remoteTestHost(t)
	stream := outboundRawPlugin(t, []byte(fakeManifest))

	adapter, sess, err := DialStream(context.Background(), h, stream, DialOptions{WantID: "fake.demo"})
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	sess.OnClosed(func(err error) {
		_, _ = h.Remove("fake.demo", false)
	})
	if err := h.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 框架 → 插件:函数调用(假插件回显)
	out, err := h.Call(context.Background(), "fake.demo", "echo", json.RawMessage(`{"hello":1}`))
	if err != nil || string(out) != `{"hello":1}` {
		t.Fatalf("Call(echo) = %s, %v", out, err)
	}
	// 工具(raw 面:tctx 即线上 JSON)
	tfn, ok := adapter.ToolRaw("t1")
	if !ok {
		t.Fatal("ToolRaw(t1) absent")
	}
	tout, err := tfn(context.Background(), json.RawMessage("null"), json.RawMessage(`{"q":1}`))
	if err != nil || tout != `"tool-out"` {
		t.Fatalf("tool = %s, %v", tout, err)
	}
	// 配置下发
	if err := h.SetConfig("fake.demo", json.RawMessage(`{"k":1}`)); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	// 保活
	if err := sess.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// 对端断开 → OnClosed(此处测试主动断)与卸载
	sess.Close()
	waitFor(t, 2*time.Second, func() bool { return !h.HasPlugin("fake.demo") })
	if _, err := h.Call(context.Background(), "fake.demo", "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("call after unload should fail")
	}
}

func TestDialWantIDMismatch(t *testing.T) {
	h, _ := remoteTestHost(t)
	stream := outboundRawPlugin(t, []byte(fakeManifest))
	if _, _, err := DialStream(context.Background(), h, stream, DialOptions{WantID: "other.id"}); err == nil || !strings.Contains(err.Error(), "!= configured") {
		t.Fatalf("mismatch err = %v", err)
	}
}

// hangingStream 模拟从不回应的对端(Recv 永不返回):用于验证 Request 超时后
// 清理 pending,防止挂起的请求条目无界累积。
type hangingStream struct{ ctx context.Context }

func (hangingStream) Send([]byte) error          { return nil }
func (hangingStream) Recv() ([]byte, error)      { select {} }
func (h hangingStream) Context() context.Context { return h.ctx }
func (hangingStream) Close() error               { return nil }

func TestRequestTimeoutClearsPending(t *testing.T) {
	s := NewSession(hangingStream{ctx: context.Background()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Request(ctx, &Frame{Kind: KindPing}); err == nil {
		t.Fatal("expected timeout error")
	}
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending entries leaked after timeout: %d", n)
	}
}

func TestInboundHookRegisterAndTrigger(t *testing.T) {
	// remote 插件注册 hook 与 Go 侧 OnHook 同构:经 on_hook HostCall 注册,
	// 框架 TriggerHook 时把 hook 帧发回插件,插件返回 HookResult JSON。
	h, _ := remoteTestHost(t)
	mf := loadManifest(t, remoteDemoManifest)

	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		},
		Hook: func(ctx context.Context, hookID string, data json.RawMessage) any {
			return contract.HookResult{Block: true, Reason: "from remote plugin", Data: json.RawMessage(`{"n":7}`)}
		},
	}
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.demo") })

	if _, err := sess.HostCall(context.Background(), OpOnHook, json.RawMessage(`{"hook_id":"demo.hook"}`)); err != nil {
		t.Fatalf("on_hook: %v", err)
	}

	results := h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{"x":1}`))
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	r := results[0]
	if !r.Block || r.Reason != "from remote plugin" || string(r.Data) != `{"n":7}` {
		t.Fatalf("hook result = %+v", r)
	}
	if r.Origin == nil || r.Origin.ID != "remote.demo" || r.Seq != 1 || r.Origin.At == 0 {
		t.Fatalf("framework fields origin=%+v seq=%d", r.Origin, r.Seq)
	}
}

// cfgProvider 是可下发配置的进程内插件(供跨插件配置/metadata 回程测试用)。
type cfgProvider struct {
	meta contract.Meta
}

func (c *cfgProvider) Meta() contract.Meta               { return c.meta }
func (c *cfgProvider) ApplyConfig(json.RawMessage) error { return nil }

func TestInboundConfigAndMetaOps(t *testing.T) {
	// 远程插件的 HostCall 回程:get_plugin/plugins(plugin.meta)与 get_config/set_config
	// (config.read/config.write + 字段 Readable/Writable)整段门控。
	h, _ := remoteTestHost(t)

	target := &cfgProvider{meta: contract.Meta{ID: "svc.cfg", Name: "Cfg", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{
			{Key: "api_key", Writable: true, Readable: false}, // 只写
			{Key: "public", Writable: false, Readable: true},  // 只读
		}}}}
	if err := h.Register(target); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("svc.cfg", json.RawMessage(`{"api_key":"sk-1","public":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&cfgProvider{meta: contract.Meta{ID: "svc.meta", Name: "Meta", Version: "2"}}); err != nil {
		t.Fatal(err)
	}

	mf := loadManifest(t, `{"id":"remote.cfg","name":"Cfg Remote","version":"1","provides":{"capabilities":["config.read","config.write","plugin.meta"]}}`)
	sess, err := AttachStream(context.Background(), inboundPipe(t, h, ""), mf, &PluginOps{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	waitFor(t, 2*time.Second, func() bool { return h.HasPlugin("remote.cfg") })

	// get_plugin:读某插件元数据。
	out, err := sess.HostCall(context.Background(), OpGetPlugin, json.RawMessage(`{"plugin_id":"svc.meta"}`))
	if err != nil {
		t.Fatalf("get_plugin: %v", err)
	}
	var gp struct {
		Found bool          `json:"found"`
		Meta  contract.Meta `json:"meta"`
	}
	if err := json.Unmarshal(out, &gp); err != nil || !gp.Found || gp.Meta.ID != "svc.meta" || gp.Meta.Version != "2" {
		t.Fatalf("get_plugin = %s (%v)", out, err)
	}

	// plugins:已加载插件列表。
	out, err = sess.HostCall(context.Background(), OpPlugins, json.RawMessage("null"))
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}
	var pls []contract.Meta
	if err := json.Unmarshal(out, &pls); err != nil {
		t.Fatalf("plugins unmarshal: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range pls {
		seen[m.ID] = true
	}
	for _, want := range []string{"svc.cfg", "svc.meta", "remote.cfg"} {
		if !seen[want] {
			t.Fatalf("plugins missing %q (got %v)", want, pls)
		}
	}

	// get_config:readable=true 字段可读;readable=false 不可读。
	out, err = sess.HostCall(context.Background(), OpGetConfig, json.RawMessage(`{"plugin_id":"svc.cfg","key":"public"}`))
	if err != nil {
		t.Fatalf("get_config public: %v", err)
	}
	var gc struct {
		Found bool            `json:"found"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(out, &gc); err != nil || !gc.Found || string(gc.Value) != `"hello"` {
		t.Fatalf("get_config public = %s (%v)", out, err)
	}
	out, _ = sess.HostCall(context.Background(), OpGetConfig, json.RawMessage(`{"plugin_id":"svc.cfg","key":"api_key"}`))
	_ = json.Unmarshal(out, &gc)
	if gc.Found {
		t.Fatalf("api_key (readable=false) should be unreadable: %s", out)
	}

	// set_config:writable=true 字段可写;writable=false 报错。
	if _, err := sess.HostCall(context.Background(), OpSetConfig, json.RawMessage(`{"plugin_id":"svc.cfg","key":"api_key","value":"sk-2"}`)); err != nil {
		t.Fatalf("set_config api_key: %v", err)
	}
	if v, _ := h.GetConfig("svc.cfg", "api_key"); string(v) != `"sk-2"` {
		t.Fatalf("api_key after write = %s", v)
	}
	if _, err := sess.HostCall(context.Background(), OpSetConfig, json.RawMessage(`{"plugin_id":"svc.cfg","key":"public","value":"nope"}`)); err == nil {
		t.Fatal("public (writable=false) should reject writes")
	}
}
