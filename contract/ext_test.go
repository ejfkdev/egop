package contract

// Extensions 自由键值缝(Ext 助手/CloneMeta 深拷贝/各声明结构的 JSON 往返/
// 框架主题判定)的契约级测试。

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtDecode(t *testing.T) {
	ext := map[string]json.RawMessage{
		"n":   json.RawMessage(`42`),
		"s":   json.RawMessage(`"str"`),
		"obj": json.RawMessage(`{"a":1}`),
		"bad": json.RawMessage(`{not-json`),
	}
	if v, ok := Ext[int](ext, "n"); !ok || v != 42 {
		t.Fatalf("Ext[int] n = %v %v", v, ok)
	}
	if v, ok := Ext[string](ext, "s"); !ok || v != "str" {
		t.Fatalf("Ext[string] s = %v %v", v, ok)
	}
	type obj struct {
		A int `json:"a"`
	}
	if v, ok := Ext[obj](ext, "obj"); !ok || v.A != 1 {
		t.Fatalf("Ext[obj] obj = %+v %v", v, ok)
	}
	if _, ok := Ext[int](ext, "missing"); ok {
		t.Fatal("missing key must be false")
	}
	if _, ok := Ext[int](ext, "bad"); ok {
		t.Fatal("bad json must be false")
	}
	if _, ok := Ext[int](nil, "n"); ok {
		t.Fatal("nil map must be false")
	}
}

func TestCloneMetaDeep(t *testing.T) {
	m := Meta{
		ID: "clone.src", Name: "C", Version: "1",
		Authors: []string{"a"},
		Tags:    []string{"t"},
		Provides: Provides{
			Points:       []string{"p"},
			Capabilities: []string{"c"},
			Functions: []FuncSpec{{
				Name:       "run",
				Input:      json.RawMessage(`{"x":1}`),
				Output:     json.RawMessage(`{"y":2}`),
				Extensions: map[string]json.RawMessage{"fk": json.RawMessage(`1`)},
			}},
			Hooks: []HookPointSpec{{
				ID: "h", Kind: KindObserve,
				Description: "d",
				Payload:     json.RawMessage(`{}`),
				Result:      json.RawMessage(`{}`),
				Extensions:  map[string]json.RawMessage{"hk": json.RawMessage(`2`)},
			}},
			Events: []EventTopicSpec{{
				ID: "e", Payload: json.RawMessage(`{}`),
				Extensions: map[string]json.RawMessage{"ek": json.RawMessage(`3`)},
			}},
			Config: []ConfigFieldSpec{{
				Key: "k", Schema: json.RawMessage(`{}`), Default: json.RawMessage(`{}`),
				Extensions: map[string]json.RawMessage{"ck": json.RawMessage(`4`)},
			}},
		},
		Requires: Requires{
			Listens: []string{"l"},
			Tools:   []string{"tl"},
			Deps: []Dependency{{
				Plugin: "dep", Kind: DepInit,
				Extensions: map[string]json.RawMessage{"dk": json.RawMessage(`5`)},
			}},
		},
		Extensions: map[string]json.RawMessage{"mk": json.RawMessage(`6`)},
	}
	c := CloneMeta(m)
	// 改写原件的一切引用部件;克隆必须不受影响。
	m.Authors[0] = "mut"
	m.Tags[0] = "mut"
	m.Provides.Points[0] = "mut"
	m.Provides.Capabilities[0] = "mut"
	m.Provides.Functions[0].Input[0] = '!'
	m.Provides.Functions[0].Extensions["fk"] = json.RawMessage(`mut`)
	m.Provides.Hooks[0].Payload[0] = '!'
	m.Provides.Hooks[0].Extensions["hk"] = json.RawMessage(`mut`)
	m.Provides.Events[0].Payload[0] = '!'
	m.Provides.Events[0].Extensions["ek"] = json.RawMessage(`mut`)
	m.Provides.Config[0].Schema[0] = '!'
	m.Provides.Config[0].Extensions["ck"] = json.RawMessage(`mut`)
	m.Requires.Deps[0].Extensions["dk"] = json.RawMessage(`mut`)
	m.Requires.Listens[0] = "mut"
	m.Requires.Tools[0] = "mut"
	m.Extensions["mk"] = json.RawMessage(`mut`)

	if c.Authors[0] != "a" || c.Tags[0] != "t" ||
		c.Provides.Points[0] != "p" || c.Provides.Capabilities[0] != "c" ||
		c.Requires.Listens[0] != "l" || c.Requires.Tools[0] != "tl" {
		t.Fatalf("string slices drifted: %+v", c)
	}
	if string(c.Provides.Functions[0].Input) != `{"x":1}` ||
		string(c.Provides.Functions[0].Extensions["fk"]) != "1" {
		t.Fatalf("function spec drifted: %+v", c.Provides.Functions[0])
	}
	if string(c.Provides.Hooks[0].Payload) != `{}` ||
		string(c.Provides.Hooks[0].Extensions["hk"]) != "2" {
		t.Fatalf("hook spec drifted: %+v", c.Provides.Hooks[0])
	}
	if string(c.Provides.Events[0].Extensions["ek"]) != "3" ||
		string(c.Provides.Config[0].Extensions["ck"]) != "4" ||
		string(c.Requires.Deps[0].Extensions["dk"]) != "5" ||
		string(c.Extensions["mk"]) != "6" {
		t.Fatalf("extensions drifted")
	}
	// 克隆后值语义等价(DeepEqual 同形状)。
	again := CloneMeta(c)
	if !reflect.DeepEqual(again, c) {
		t.Fatal("clone of clone must be equal")
	}
}

func TestSpecExtensionsRoundTrip(t *testing.T) {
	// 一份带全部 Extensions 轴的线上清单 JSON:解析后各 spec 的 Extensions 落位。
	raw := []byte(`{
		"id": "rt.src", "name": "R", "version": "1",
		"extensions": {"vendor.x": {"a": 1}},
		"provides": {
			"hooks": [{"id": "h", "kind": "observe", "description": "d", "extensions": {"vendor.h": true}}],
			"events": [{"id": "e", "extensions": {"vendor.e": [1, 2]}}],
			"functions": [{"name": "run", "extensions": {"vendor.f": "s"}}],
			"config": [{"key": "k", "secret": true, "extensions": {"vendor.c": 9}}]
		},
		"requires": {
			"deps": [{"plugin": "dep", "kind": "init", "extensions": {"vendor.d": "x"}}]
		}
	}`)
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := Ext[map[string]any](m.Extensions, "vendor.x"); !ok || v["a"].(float64) != 1 {
		t.Fatalf("meta extensions: %v %v", v, ok)
	}
	if v, ok := Ext[bool](m.Provides.Hooks[0].Extensions, "vendor.h"); !ok || !v {
		t.Fatalf("hook extensions: %v %v", v, ok)
	}
	if v, ok := Ext[[]int](m.Provides.Events[0].Extensions, "vendor.e"); !ok || len(v) != 2 {
		t.Fatalf("event extensions: %v %v", v, ok)
	}
	if v, ok := Ext[string](m.Provides.Functions[0].Extensions, "vendor.f"); !ok || v != "s" {
		t.Fatalf("function extensions: %v %v", v, ok)
	}
	if !m.Provides.Config[0].Secret {
		t.Fatal("secret flag lost")
	}
	if v, ok := Ext[int](m.Provides.Config[0].Extensions, "vendor.c"); !ok || v != 9 {
		t.Fatalf("config extensions: %v %v", v, ok)
	}
	if v, ok := Ext[string](m.Requires.Deps[0].Extensions, "vendor.d"); !ok || v != "x" {
		t.Fatalf("dependency extensions: %v %v", v, ok)
	}
	if m.Provides.Hooks[0].Description != "d" {
		t.Fatalf("hook description (原 Desc) lost: %+v", m.Provides.Hooks[0])
	}
	// SlotSpec 同样往返。
	var s SlotSpec
	if err := json.Unmarshal([]byte(`{"id":"sl","doc":"d","builtin":true,"extensions":{"vendor.s":1}}`), &s); err != nil {
		t.Fatal(err)
	}
	if v, ok := Ext[int](s.Extensions, "vendor.s"); !ok || v != 1 {
		t.Fatalf("slot extensions: %v %v", v, ok)
	}
}

func TestIsFrameworkTopic(t *testing.T) {
	for _, topic := range []string{
		EventConfigUpdated, EventPluginRegistered, EventPluginRemoved,
		EventPluginReplaced, EventPluginFailed,
	} {
		if !IsFrameworkTopic(topic) {
			t.Fatalf("%q must be framework-reserved", topic)
		}
	}
	for _, topic := range []string{"plugin.custom", "chat.msg", "", "dyn.a.b"} {
		if IsFrameworkTopic(topic) {
			t.Fatalf("%q must not be reserved", topic)
		}
	}
}
