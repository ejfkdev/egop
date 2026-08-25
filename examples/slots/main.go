// 槽位契约示例(Slot/SlotSpec):把"某个能力 id → 最小契约"注册进 SlotLookup(八轴),
// 插件用 Meta.Slot 声明实现了该 id,宿主逐轴求差(只多不少),不满足即拒注。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// incomplete 声称实现 demo.greeter 槽位,但缺契约要求的事件主题与函数 → 会被拒。
type incomplete struct{}

func (incomplete) Meta() contract.Meta {
	return contract.Meta{ID: "slot.incomplete", Name: "Incomplete", Version: "1", Slot: "demo.greeter"}
}

// greeter 满足 demo.greeter 槽位契约(发布事件主题 + 提供函数)。
type greeter struct{}

func (greeter) Meta() contract.Meta {
	return contract.Meta{
		ID:      "slot.greeter",
		Name:    "Greeter",
		Version: "1",
		Slot:    "demo.greeter",
		Provides: contract.Provides{
			Events:    []contract.EventTopicSpec{{ID: "greeted"}},
			Functions: []contract.FuncSpec{{Name: "greet"}},
		},
	}
}

func (greeter) CallFunc(_ context.Context, fname string, _ json.RawMessage) (json.RawMessage, error) {
	if fname == "greet" {
		return json.RawMessage(`"hello"`), nil
	}
	return nil, fmt.Errorf("greeter: unknown function %q", fname)
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{
		Logf: log.Printf,
		// 注册"能力 id → 最小契约"。声称实现该 id 的插件必须逐轴满足(只多不少)。
		SlotLookup: func(id string) (contract.SlotSpec, bool) {
			specs := map[string]contract.SlotSpec{
				"demo.greeter": {
					ID:        "demo.greeter",
					Doc:       "对外提供问候能力",
					Events:    []string{"greeted"},
					Functions: []string{"greet"},
				},
			}
			s, ok := specs[id]
			return s, ok
		},
	})

	// 声称实现但契约不满足(缺事件主题 + 缺函数)→ 拒注,错误逐轴列出。
	if err := h.Register(incomplete{}); err != nil {
		log.Printf("claim rejected: %v", err)
	}

	// 满足契约 → 接受,并按契约调用函数。
	if err := h.Register(&greeter{}); err != nil {
		log.Fatal(err)
	}
	out, err := h.Call(ctx, "slot.greeter", "greet", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("slot.greeter.greet() = %s", out)

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("host closed")
}
