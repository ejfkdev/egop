// Hook 派发示例:OnHook 注册回调、TriggerHook 触发。回调可返回带"阻断/理由/产出"
// 的 HookResult(Block/Reason/Data),也可直接返回数据(egop 归一成 HookResult),
// 框架统一回填 Origin(Kind:"hook")/Seq。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	// 第一个回调:观察,直接返回数据(egop 会包成 HookResult{Data:...})。
	_ = h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
		return map[string]any{"normalized": true}
	})
	// 第二个回调:带描述性理由地阻断(返回完整 HookResult)。
	_ = h.OnHook("demo.validate", func(ctx context.Context, hookID string, data json.RawMessage) any {
		return contract.HookResult{
			Block:  true,
			Reason: "payload denied by policy",
			Data:   json.RawMessage(`{"policy":"deny-all"}`),
		}
	})

	results := h.TriggerHook(ctx, "demo.validate", json.RawMessage(`{"n":1}`))
	for i, r := range results {
		who, at := "", int64(0)
		if r.Origin != nil {
			who, at = r.Origin.ID, r.Origin.At
		}
		fmt.Printf("[%d] seq=%d who=%q at=%d block=%v reason=%q data=%s\n",
			i, r.Seq, who, at, r.Block, r.Reason, r.Data)
	}

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
}
