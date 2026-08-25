// 槽位契约演示:把"某个槽位 id + 最小契约"注册进 SlotLookup,声称实现它的插件
// 必须逐轴满足(SlotSpec 八轴,只多不少)。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// slotBad 声称实现 demo.greeter 槽位,但缺少契约要求的事件主题与函数 → 会被拒。
type slotBad struct{}

func (slotBad) Meta() contract.Meta {
	return contract.Meta{ID: "slot.bad", Name: "Bad", Version: "1", Slot: "demo.greeter"}
}

// slotOk 满足 demo.greeter 槽位契约(发布事件主题 + 提供函数)。
type slotOk struct{}

func (slotOk) Meta() contract.Meta {
	return contract.Meta{
		ID:      "slot.ok",
		Name:    "Ok",
		Version: "1",
		Slot:    "demo.greeter",
		Provides: contract.Provides{
			Events:    []contract.EventTopicSpec{{ID: "run.begin"}},
			Functions: []contract.FuncSpec{{Name: "ping"}},
		},
	}
}

func (slotOk) CallFunc(_ context.Context, fname string, _ json.RawMessage) (json.RawMessage, error) {
	if fname == "ping" {
		return json.RawMessage(`"pong"`), nil
	}
	return nil, fmt.Errorf("slot.ok: unknown function %q", fname)
}
