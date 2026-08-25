// 懒注册 + 批量装载示例:依赖"后到"也能自动补载;批量乱序按 DepInit 拓扑排序。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// base 是被依赖方(提供 add)。
type base struct{}

func (base) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.base", Name: "Base", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "add"}}},
	}
}
func (base) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`42`), nil
}

// consumer 依赖 demo.base:声明 DepInit 依赖;懒注册时 base 未到 → 转待补载。
type consumer struct{ surface contract.Surface }

func (c *consumer) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.consumer", Name: "Consumer", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapCallPlugins},
			Functions:    []contract.FuncSpec{{Name: "sum"}},
		},
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "demo.base", Kind: contract.DepInit}}},
	}
}
func (c *consumer) SetSurface(s contract.Surface) { c.surface = s }
func (c *consumer) CallFunc(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return c.surface.Call(ctx, "demo.base", "add", json.RawMessage(`{}`))
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	// 懒注册:依赖未到 → StatusPending,不报错。
	st, err := h.RegisterLazy(&consumer{})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("RegisterLazy(consumer) = %s (base 未到,转入待补载)", st)

	// base 后到:自动补载 consumer(含传递链)。
	if err := h.Register(base{}); err != nil {
		log.Fatal(err)
	}
	log.Printf("Register(base) 触发自动补载:consumer 在册=%v", h.HasPlugin("demo.consumer"))

	out, _ := h.Call(ctx, "demo.consumer", "sum", json.RawMessage(`{}`))
	log.Printf("consumer.sum() = %s", out)

	// 批量装载:乱序传入也能按依赖拓扑排序(失败隔离;缺依赖的件转 Pending)。
	h2 := host.New[any](host.Options[any]{})
	rep := h2.RegisterMany([]contract.Plugin{&consumer{}, base{}})
	log.Printf("RegisterMany => registered %v, pending %v, failed %d", rep.Registered, rep.Pending, len(rep.Failed))

	_ = h.Close(ctx)
	_ = h2.Close(ctx)
}
