// 依赖与跨插件调用示例:进程内用代码注册两个插件,calc.sigma 依赖且调用
// calc.basic。运行:go run .
//
// 演示点:
//   - 依赖顺序敏感:init 依赖未满足时注册被拒;
//   - 能力门控下跨插件调用(contract.Surface.Call,须声明 plugin.call);
//   - 依赖反查(Host.Dependents)与 fail-closed / 级联卸载;
//   - 批量装载 Host.RegisterMany:自动按依赖拓扑排序、失败隔离;
//   - 槽位契约:SlotLookup 注册 id→契约,声称实现者逐轴求差(八轴);
//   - Hook 派发:OnHook 注册回调,TriggerHook 触发并收集 HookResult(Block/Data)。
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// ghostCalc 依赖一个不存在的插件,用于演示批量装载里的"缺依赖隔离"。
type ghostCalc struct{}

func (ghostCalc) Meta() contract.Meta {
	return contract.Meta{
		ID:      "calc.ghost",
		Name:    "Ghost",
		Version: "1",
		Requires: contract.Requires{
			Deps: []contract.Dependency{
				{Plugin: "calc.missing", Kind: contract.DepInit},
			},
		},
	}
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	basic := &basicCalc{}
	sigma := &sigmaCalc{}

	// 顺序敏感:先注册依赖方(calc.sigma)会因 init 依赖 calc.basic 未在册被拒。
	if err := h.Register(sigma); err != nil {
		log.Printf("register sigma before basic refused: %v", err)
	}

	// 正确顺序:被依赖方先注册,依赖方后注册。
	if err := h.Register(basic); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(sigma); err != nil {
		log.Fatal(err)
	}

	// 直接调用被依赖方。
	addOut, err := h.Call(ctx, "calc.basic", "add", json.RawMessage(`{"a":20,"b":22}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("calc.basic.add(20,22) = %s", addOut)

	// 调用依赖方:sum 内部会经 Surface.Call 跨插件调用 calc.basic.add。
	sumOut, err := h.Call(ctx, "calc.sigma", "sum", json.RawMessage(`{"nums":[3,4,5]}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("calc.sigma.sum([3,4,5]) = %s (内部 3 次跨插件 add)", sumOut)

	// 依赖反查:谁在依赖 calc.basic。
	log.Printf("dependents(calc.basic) = %v", h.Dependents("calc.basic"))

	// fail-closed:被依赖时非级联卸载被拒。
	if _, err := h.Remove("calc.basic", false); err != nil {
		log.Printf("remove calc.basic (non-cascade) refused: %v", err)
	}

	// 级联卸载:连带卸下依赖它的 calc.sigma。
	victims, err := h.Remove("calc.basic", true)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("cascade remove calc.basic => victims %v", victims)

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("host closed")

	// ---------- 批量装载:RegisterMany 自动按依赖拓扑排序 ----------

	// 依赖方在前、被依赖方在后——不用手工排序,框架按 Requires(DepInit) 排好再装。
	h2 := host.New[any](host.Options[any]{})
	rep := h2.RegisterMany([]contract.Plugin{&sigmaCalc{}, &basicCalc{}})
	log.Printf("RegisterMany(reversed) => registered %v", rep.Registered)
	_ = h2.Close(ctx)

	// 失败隔离:缺依赖的件单独记入 Failed,不阻断同批的其它件。
	h3 := host.New[any](host.Options[any]{})
	rep3 := h3.RegisterMany([]contract.Plugin{ghostCalc{}, &basicCalc{}})
	for _, f := range rep3.Failed {
		log.Printf("RegisterMany failed: %s: %v", f.ID, f.Err)
	}
	log.Printf("RegisterMany(partial) => registered %v", rep3.Registered)
	_ = h3.Close(ctx)

	// ---------- 槽位契约:注册"槽位 id → 最小契约",声称实现者逐轴求差 ----------

	h4 := host.New[any](host.Options[any]{
		SlotLookup: func(id string) (contract.SlotSpec, bool) {
			if id != "demo.greeter" {
				return contract.SlotSpec{}, false
			}
			return contract.SlotSpec{
				ID:        "demo.greeter",
				Doc:       "示例业务槽位",
				Events:    []string{"run.begin"},
				Functions: []string{"ping"},
			}, true
		},
	})

	// 声称实现但契约不满足(缺事件主题 + 缺函数)→ 拒注。
	if err := h4.Register(slotBad{}); err != nil {
		log.Printf("slot claim rejected (contract unmet): %v", err)
	}
	// 满足契约 → 接受并可按契约调用。
	if err := h4.Register(slotOk{}); err != nil {
		log.Fatal(err)
	}
	ping, err := h4.Call(context.Background(), "slot.ok", "ping", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("slot.ok.ping() = %s", ping)
	_ = h4.Close(ctx)

	// ---------- Hook 派发:OnHook 注册回调,TriggerHook 触发并收集 HookResult ----------

	h5 := host.New[any](host.Options[any]{})
	unsub := h5.OnHook("demo.validate", func(_ context.Context, _ string, data json.RawMessage) any {
		log.Printf("validate hook sees %s", data)
		// 返回值同时携带"是否阻断"与"产出数据"——阻断不是靠着 nil/false 猜。
		return contract.HookResult{Block: true, Data: json.RawMessage(`{"reason":"not allowed"}`)}
	})
	for _, r := range h5.TriggerHook(context.Background(), "demo.validate", json.RawMessage(`{"n":1}`)) {
		if r.Block {
			log.Printf("hook blocked: %s", r.Data)
		}
	}
	unsub() // 撤销(插件侧注册的 hook 则在插件卸载时被宿主自动回滚)
	_ = h5.Close(ctx)
}
