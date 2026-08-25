// 宿主控制面(元数据反查)的直接测试:Dependents / CapabilityIndex / Functions
// 三个反查口与 Surface 的 GetSetting(无门控)/ Exec(exec.cmd 门控)语义。
package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestDependentsControlPlane(t *testing.T) {
	h := New[any](Options[any]{})
	for _, p := range []contract.Plugin{
		&demoPlugin{meta: contract.Meta{ID: "a", Name: "A", Version: "1"}},
		&demoPlugin{meta: contract.Meta{ID: "b", Name: "B", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "a", Kind: contract.DepInit}}}}},
		&demoPlugin{meta: contract.Meta{ID: "c", Name: "C", Version: "1", Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "a", Kind: contract.DepInit}}}}},
		&demoPlugin{meta: contract.Meta{ID: "d", Name: "D", Version: "1"}},
	} {
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	// 反查顺序不定(遍历 meta map),只断言集合。
	got := h.Dependents("a")
	if len(got) != 2 {
		t.Fatalf("Dependents(a) = %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["b"] || !seen["c"] {
		t.Fatalf("Dependents(a) should contain b,c: %v", got)
	}
	if got := h.Dependents("d"); len(got) != 0 {
		t.Fatalf("Dependents(d) should be empty: %v", got)
	}
}

func TestCapabilityIndexControlPlane(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "p1", Name: "P1", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapCallPlugins, contract.CapNet}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "p2", Name: "P2", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapNet}}}}); err != nil {
		t.Fatal(err)
	}
	idx := h.CapabilityIndex()
	if got := idx[contract.CapNet]; len(got) != 2 {
		t.Fatalf("CapabilityIndex[net.access] = %v", got)
	}
	if got := idx[contract.CapCallPlugins]; len(got) != 1 || got[0] != "p1" {
		t.Fatalf("CapabilityIndex[plugin.call] = %v", got)
	}
	if got := idx[contract.CapExec]; len(got) != 0 {
		t.Fatalf("CapabilityIndex[exec.cmd] should be empty: %v", got)
	}
}

func TestFunctionsControlPlane(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "p1", Name: "P1", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "f2"}, {Name: "f1"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "p2", Name: "P2", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "g1"}}}}}); err != nil {
		t.Fatal(err)
	}
	fns := h.Functions()
	if len(fns) != 3 {
		t.Fatalf("Functions() = %+v", fns)
	}
	// 按 "plugin.fn" 键字典序,与声明顺序无关。
	keys := make([]string, 0, 3)
	for _, f := range fns {
		keys = append(keys, f.PluginID+"."+f.Spec.Name)
	}
	if keys[0] != "p1.f1" || keys[1] != "p1.f2" || keys[2] != "p2.g1" {
		t.Fatalf("Functions order = %v", keys)
	}
}

func TestSurfaceGetSettingUngated(t *testing.T) {
	s := NewMapSettings()
	s.Set("app.name", json.RawMessage(`"egop"`))
	h := New[any](Options[any]{Settings: s})
	// 未声明任何能力,仍可读宿主 setting(GetSetting 非能力门控,是共享环境值)。
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "plain", Name: "Plain", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	sur, _ := h.SurfaceFor("plain")
	v, found := sur.GetSetting("app.name")
	if !found || string(v) != `"egop"` {
		t.Fatalf("GetSetting = %s, %v", v, found)
	}
	if _, found := sur.GetSetting("missing"); found {
		t.Fatal("missing setting should be not-found")
	}
}

func TestSurfaceExecGated(t *testing.T) {
	h := New[any](Options[any]{ExecFn: func(_ context.Context, cmd string) (string, error) { return "ran:" + cmd, nil }})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "runner", Name: "R", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapExec}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "plain", Name: "P", Version: "1"}}); err != nil {
		t.Fatal(err)
	}

	rs, _ := h.SurfaceFor("runner")
	if out, err := rs.Exec(context.Background(), "id"); err != nil || out != "ran:id" {
		t.Fatalf("declared Exec = %q, %v", out, err)
	}

	ps, _ := h.SurfaceFor("plain")
	if _, err := ps.Exec(context.Background(), "id"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared Exec err = %v", err)
	}
}

func TestSurfacePublishRequiresTopic(t *testing.T) {
	bus := NewMemEvents()
	h := New[any](Options[any]{Events: bus})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "emitter", Name: "E", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapEmitsEvents}}}}); err != nil {
		t.Fatal(err)
	}
	// 匹配一切的总线订阅,用来观察"哪些事件真的被投递"。
	var got []contract.Event
	bus.Subscribe(nil, func(_ context.Context, e contract.Event) { got = append(got, e) })

	sur, _ := h.SurfaceFor("emitter")
	sur.Publish(context.Background(), contract.Event{Type: "", Payload: json.RawMessage(`{}`)}) // 主题必填:空则丢弃
	sur.PublishEvent(context.Background(), "", json.RawMessage(`{}`))                           // 同样丢弃
	sur.PublishEvent(context.Background(), "real.topic", json.RawMessage(`{}`))                 // 有主题 → 投递

	if len(got) != 1 || got[0].Type != "real.topic" {
		t.Fatalf("empty-topic publish should be dropped; got %+v", got)
	}
}
