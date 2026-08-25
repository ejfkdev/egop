// examples/origin 冒烟:事件 e.Source 与跨插件调用 contract.OriginFrom(ctx) 两路溯源。
package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

func TestExampleOrigin(t *testing.T) {
	h := host.New[any](host.Options[any]{})
	if err := h.Register(&echoer{basic{contract.Meta{ID: "echoer", Name: "E", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "who"}}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&basic{contract.Meta{ID: "pub", Name: "P", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapEmitsEvents}}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(&basic{contract.Meta{ID: "obs", Name: "O", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapListensEvents, contract.CapCallPlugins}}}}); err != nil {
		t.Fatal(err)
	}

	os, _ := h.SurfaceFor("obs")
	var got *contract.Origin
	os.SubscribeEvent("topic.src", func(_ context.Context, _ string, e contract.Event) {
		got = e.Source
	})
	ps, _ := h.SurfaceFor("pub")
	ps.PublishEvent(context.Background(), "topic.src", json.RawMessage(`{}`))
	if got == nil || got.ID != "pub" || got.Kind != contract.OriginEvent || got.Point != "topic.src" {
		t.Fatalf("event origin = %+v (want id=pub kind=event point=topic.src)", got)
	}

	out, err := os.Call(context.Background(), "echoer", "who", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var caller map[string]string
	if err := json.Unmarshal(out, &caller); err != nil {
		t.Fatal(err)
	}
	if caller["caller"] != "obs" || caller["kind"] != "call" || caller["point"] != "who" {
		t.Fatalf("callee saw caller = %v (want caller=obs kind=call point=who)", caller)
	}
}
