// 能力面示例:先说后做——插件声明 capability 才能经 Surface 调用对应能力;
// 扩展能力经 Op(name,input) 自由调用,OpAliases 把 wire 短名映射到守卫词。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// policied 声明了 demo.policy 能力,经 Surface.Op 调自定义扩展能力。
type policied struct{ surface contract.Surface }

func (p *policied) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.policied", Name: "Policied", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{"demo.policy"},
			Functions:    []contract.FuncSpec{{Name: "apply"}},
		},
	}
}
func (p *policied) SetSurface(s contract.Surface) { p.surface = s }
func (p *policied) CallFunc(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	// 扩展能力:Op 经 OpAliases 把 wire 短名 apply_policy 映射到守卫词 demo.policy。
	return p.surface.Op(ctx, "apply_policy", json.RawMessage(`{"doc":"x"}`))
}

// naked 不声明 demo.policy,却想调同一个 op → 应被拒。
type naked struct{ surface contract.Surface }

func (n *naked) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.naked", Name: "Naked", Version: "1",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{{Name: "apply"}},
		},
	}
}
func (n *naked) SetSurface(s contract.Surface) { n.surface = s }
func (n *naked) CallFunc(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return n.surface.Op(ctx, "apply_policy", json.RawMessage(`{"doc":"x"}`))
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{
		Logf: log.Printf,
		// 扩展能力处理器:守卫能力词 → 处理器
		Ops: map[string]host.Op{
			"demo.policy": func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"decision":"allow"}`), nil
			},
		},
		// wire 短名 → 守卫能力词
		OpAliases: map[string]string{"apply_policy": "demo.policy"},
	})

	if err := h.Register(&policied{}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&naked{}); err != nil {
		log.Fatal(err)
	}

	out, err := h.Call(ctx, "demo.policied", "apply", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("policied.apply() = %s", out)

	// 未声明 demo.policy 能力 → Op 被拒。
	if _, err := h.Call(ctx, "demo.naked", "apply", json.RawMessage(`{}`)); err != nil {
		log.Printf("naked.apply() rejected: %v", err)
	}

	_ = h.Close(ctx)
}
