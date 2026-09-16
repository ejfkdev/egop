package host

// 契约扩展缝(Extensions/注册快照/键校验/框架主题/observe hook/Secret 脱敏)
// 的机制级测试。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// ---- 测试插件形状 ----

// extMetaPlugin 可变 Meta 的进程内插件(注册快照测试:注册后改写自己的 Meta)。
type extMetaPlugin struct{ meta contract.Meta }

func (p *extMetaPlugin) Meta() contract.Meta { return p.meta }

// extPubPlugin 拿到注入 Surface 的插件(框架主题保留测试)。
type extPubPlugin struct {
	meta  contract.Meta
	surf  contract.Surface
	logMu sync.Mutex
}

func (p *extPubPlugin) Meta() contract.Meta           { return p.meta }
func (p *extPubPlugin) SetSurface(s contract.Surface) { p.surf = s }

// extConfPlugin 记录 ApplyConfig 收到的配置(Secret 脱敏测试)。
type extConfPlugin struct {
	meta contract.Meta
	got  []json.RawMessage
}

func (p *extConfPlugin) Meta() contract.Meta { return p.meta }
func (p *extConfPlugin) ApplyConfig(cfg json.RawMessage) error {
	p.got = append(p.got, cfg)
	return nil
}

// ---- 注册快照:入册后插件侧改写 Meta 不影响宿主视图 ----

func TestRegisterSnapshotsMeta(t *testing.T) {
	h := New[any](Options[any]{})
	raw := json.RawMessage(`{"x":1}`)
	p := &extMetaPlugin{meta: contract.Meta{
		ID: "snap.src", Name: "S", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{"plugin.meta"},
			Functions:    []contract.FuncSpec{{Name: "run", Input: raw}},
		},
		Extensions: map[string]json.RawMessage{"k": json.RawMessage(`1`)},
	}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	// 注册后改写插件侧 Meta(引用共享是旧世界的坑;快照后必须无影响)。
	p.meta.Provides.Capabilities = append(p.meta.Provides.Capabilities, "event.emit")
	p.meta.Extensions["k"] = json.RawMessage(`2`)
	p.meta.Provides.Functions[0].Input[0] = '!' // 原 RawMessage 字节被改写

	var m contract.Meta
	found := false
	for _, pm := range h.Plugins() {
		if pm.ID == "snap.src" {
			m, found = pm, true
		}
	}
	if !found {
		t.Fatal("not registered")
	}
	if len(m.Provides.Capabilities) != 1 || m.Provides.Capabilities[0] != "plugin.meta" {
		t.Fatalf("host caps drifted: %v", m.Provides.Capabilities)
	}
	if string(m.Extensions["k"]) != "1" {
		t.Fatalf("host extensions drifted: %s", m.Extensions["k"])
	}
	if string(m.Provides.Functions[0].Input) != `{"x":1}` {
		t.Fatalf("host fn input drifted: %s", m.Provides.Functions[0].Input)
	}
}

// ---- 名称/依赖形状校验(注册口 fail-closed) ----

func TestNameAndDependencyValidation(t *testing.T) {
	h := New[any](Options[any]{})
	bad := []struct {
		name string
		meta contract.Meta
		want string
	}{
		{"id with space", contract.Meta{ID: "bad id"}, "whitespace or control"},
		{"fn name with dot", contract.Meta{ID: "vendor.ok", Provides: contract.Provides{
			Functions: []contract.FuncSpec{{Name: "a.b"}},
		}}, "function name"},
		{"fn name empty", contract.Meta{ID: "vendor.ok", Provides: contract.Provides{
			Functions: []contract.FuncSpec{{Name: ""}},
		}}, "function name"},
		{"dep without target", contract.Meta{ID: "vendor.ok", Requires: contract.Requires{
			Deps: []contract.Dependency{{Kind: contract.DepInit}},
		}}, "neither plugin nor slot"},
	}
	for _, tc := range bad {
		err := h.Register(&extMetaPlugin{meta: tc.meta})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v (want %q)", tc.name, err, tc.want)
		}
	}
	// 合法形状:id 含点(vendor.name 惯例)+ 函数名无点。
	ok := &extMetaPlugin{meta: contract.Meta{
		ID: "vendor.tool", Name: "V", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "run"}}},
	}}
	if err := h.Register(ok); err != nil {
		t.Fatalf("valid shape rejected: %v", err)
	}
}

// ---- 框架保留主题:插件不可伪造生命周期/配置观察事件 ----

func TestPublishFrameworkTopicDropped(t *testing.T) {
	bus := NewMemEvents()
	var mu sync.Mutex
	var topics []string
	bus.Subscribe(nil, func(_ context.Context, e contract.Event) {
		mu.Lock()
		topics = append(topics, e.Type)
		mu.Unlock()
	})
	var logs []string
	h := New[any](Options[any]{Events: bus, Logf: func(f string, a ...any) {
		logs = append(logs, strings.TrimSpace(f))
	}})
	p := &extPubPlugin{meta: contract.Meta{
		ID: "pub.probe", Name: "P", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapEmitsEvents}},
	}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	p.surf.PublishEvent(context.Background(), "plugin.removed", nil)
	p.surf.PublishEvent(context.Background(), contract.EventConfigUpdated, nil)
	p.surf.PublishEvent(context.Background(), "chat.msg", json.RawMessage(`{}`))
	mu.Lock()
	got := append([]string(nil), topics...)
	mu.Unlock()
	// 宿主自身的 plugin.registered 生命周期广播是合法投递;插件伪造的两个框架
	// 主题必须被丢弃。
	for _, topic := range got {
		if topic == "plugin.removed" || topic == contract.EventConfigUpdated {
			t.Fatalf("plugin-forged framework topic %q delivered", topic)
		}
	}
	delivered := false
	for _, topic := range got {
		if topic == "chat.msg" {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("normal topic must be delivered, got %v", got)
	}
	sawDropLog := false
	for _, l := range logs {
		if strings.Contains(l, "framework-reserved") {
			sawDropLog = true
		}
	}
	if !sawDropLog {
		t.Fatalf("drop must be logged, logs = %v", logs)
	}
}

// ---- observe hook:声明优先,Block 被丢弃;声明者删除/替换后重建 ----

func TestObserveHookBlockDropped(t *testing.T) {
	h := New[any](Options[any]{})
	owner := func(kind contract.HookKind) *extMetaPlugin {
		return &extMetaPlugin{meta: contract.Meta{
			ID: "hook.owner", Name: "H", Version: "1",
			Provides: contract.Provides{Hooks: []contract.HookPointSpec{
				{ID: "obs.x", Kind: kind, Description: "probe"},
			}},
		}}
	}
	blocker := func(ctx context.Context, hookID string, data json.RawMessage) any {
		return contract.HookResult{Block: true, Reason: "try to block"}
	}
	if err := h.Register(owner(contract.KindObserve)); err != nil {
		t.Fatal(err)
	}
	h.OnHook("obs.x", blocker)
	res := h.TriggerHook(context.Background(), "obs.x", json.RawMessage(`{}`))
	if len(res) != 1 || res[0].Block {
		t.Fatalf("observe point must drop block: %+v", res)
	}
	if !strings.Contains(res[0].Reason, "observe") {
		t.Fatalf("reason should note dropped block: %+v", res[0])
	}
	// 替换件把点改为 modify:Block 恢复生效(声明表重建)。
	if err := h.Replace(owner(contract.KindModify)); err != nil {
		t.Fatal(err)
	}
	res = h.TriggerHook(context.Background(), "obs.x", json.RawMessage(`{}`))
	if len(res) != 1 || !res[0].Block {
		t.Fatalf("modify point must honor block: %+v", res)
	}
	// 声明者删除:无声明按 modify(可阻断)。
	if _, err := h.Remove("hook.owner", true); err != nil {
		t.Fatal(err)
	}
	res = h.TriggerHook(context.Background(), "obs.x", json.RawMessage(`{}`))
	if len(res) != 1 || !res[0].Block {
		t.Fatalf("after owner removed block must be honored: %+v", res)
	}
}

// ---- Secret 字段声明优先脱敏(观察事件),applied 保留全值 ----

func TestSecretConfigRedaction(t *testing.T) {
	bus := NewMemEvents()
	var mu sync.Mutex
	var payloads []json.RawMessage
	bus.Subscribe(&contract.EventFilter{Type: contract.EventConfigUpdated}, func(_ context.Context, e contract.Event) {
		mu.Lock()
		payloads = append(payloads, e.Payload)
		mu.Unlock()
	})
	h := New[any](Options[any]{Events: bus})
	p := &extConfPlugin{meta: contract.Meta{
		ID: "conf.sec", Name: "C", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{
			{Key: "api_url", Secret: true}, // 键名不敏感但声明了 Secret
			{Key: "note"},                  // 非敏感
		}},
	}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	cfg := json.RawMessage(`{"api_url":"https://svc/token","note":"hello","password":"pw"}`)
	if err := h.SetConfig("conf.sec", cfg); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || string(p.got[0]) != string(cfg) {
		t.Fatalf("plugin must receive full config: %v", p.got)
	}
	applied, ok := h.AppliedConfig("conf.sec")
	if !ok || string(applied) != string(cfg) {
		t.Fatalf("applied must keep full value: %s", applied)
	}
	mu.Lock()
	pl := payloads[len(payloads)-1]
	mu.Unlock()
	var evt struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(pl, &evt); err != nil {
		t.Fatal(err)
	}
	s := string(evt.Config)
	if strings.Contains(s, "https://svc/token") || strings.Contains(s, `"pw"`) {
		t.Fatalf("secret leaked into observation event: %s", s)
	}
	if !strings.Contains(s, redactedMark) {
		t.Fatalf("declared secret must be redacted: %s", s)
	}
	if !strings.Contains(s, `"note":"hello"`) {
		t.Fatalf("non-secret field lost: %s", s)
	}
}
