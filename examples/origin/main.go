// 溯源(Origin)示例:事件订阅者经 e.Source、被调函数经 contract.OriginFrom(ctx)
// 拿到"谁 / 哪个版本 / 什么类型 / 哪个点位"。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// basic 只实现 Meta。
type basic struct{ meta contract.Meta }

func (b *basic) Meta() contract.Meta { return b.meta }

// echoer 提供 who 函数,回显调用者来源(OriginFrom)。
type echoer struct{ basic }

func (e *echoer) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	o := contract.OriginFrom(ctx)
	caller, kind, point, ver := "", "", "", ""
	if o != nil {
		caller, kind, point, ver = o.ID, string(o.Kind), o.Point, o.Version
	}
	return json.Marshal(map[string]string{"caller": caller, "kind": kind, "point": point, "version": ver})
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	if err := h.Register(&echoer{basic{contract.Meta{ID: "echoer", Name: "E", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "who"}}}}}}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&basic{contract.Meta{ID: "pub", Name: "P", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapEmitsEvents}}}}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&basic{contract.Meta{ID: "obs", Name: "O", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapListensEvents, contract.CapCallPlugins}}}}); err != nil {
		log.Fatal(err)
	}
	os, _ := h.SurfaceFor("obs")
	ps, _ := h.SurfaceFor("pub")

	// (1) 事件来源:订阅后发布,打印 e.Source。
	os.SubscribeEvent("topic.src", func(_ context.Context, _ string, e contract.Event) {
		s := e.Source
		log.Printf("event source: id=%s version=%s kind=%s point=%s", s.ID, s.Version, s.Kind, s.Point)
	})
	ps.PublishEvent(ctx, "topic.src", json.RawMessage(`{"n":1}`))

	// (2) 调用来源:obs 调 echoer.who,打印被调方看到的调用者。
	out, err := os.Call(ctx, "echoer", "who", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("call source seen by echoer: %s", out)

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
}
