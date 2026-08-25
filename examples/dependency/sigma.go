// calc.sigma:依赖 calc.basic 的插件,通过能力门控的 Surface.Call 跨插件调用它。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// sigmaCalc 是求和插件:B 依赖 A(init 依赖),并在 sum 里经 Surface.Call 调用
// calc.basic.add——跨插件调用与进程内函数调用走同一条能力门控路径。
type sigmaCalc struct {
	surface contract.Surface
}

func (s *sigmaCalc) Meta() contract.Meta {
	return contract.Meta{
		ID:      "calc.sigma",
		Name:    "Summation",
		Version: "1.0.0",
		Provides: contract.Provides{
			// 能力先说后做:跨插件调用必须先声明 plugin.call,否则 Surface.Call 报错。
			Capabilities: []string{contract.CapCallPlugins},
			Functions: []contract.FuncSpec{{
				Name:        "sum",
				Description: "输入 {nums:[...]},返回求和(内部跨插件调 add)",
				Input:       json.RawMessage(`{"type":"object","properties":{"nums":{"type":"array","items":{"type":"integer"}}},"required":["nums"]}`),
				Output:      json.RawMessage(`{"type":"integer"}`),
			}},
		},
		Requires: contract.Requires{
			// init 依赖:注册时 calc.basic 必须已在册(顺序敏感)。
			Deps: []contract.Dependency{{Plugin: "calc.basic", Kind: contract.DepInit}},
		},
	}
}

// SetSurface 实现 contract.SurfaceAware:注册后宿主注入按 Capabilities 裁剪的 Surface 视图。
func (s *sigmaCalc) SetSurface(sur contract.Surface) { s.surface = sur }

func (s *sigmaCalc) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	if fname != "sum" {
		return nil, fmt.Errorf("calc.sigma: unknown function %q", fname)
	}
	var in struct {
		Nums []int `json:"nums"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	total := 0
	for _, n := range in.Nums {
		// 跨插件调用:经 Surface.Call 打到 calc.basic.add。
		args, _ := json.Marshal(map[string]int{"a": total, "b": n})
		out, err := s.surface.Call(ctx, "calc.basic", "add", args)
		if err != nil {
			return nil, fmt.Errorf("calc.sigma: call calc.basic.add: %w", err)
		}
		var v int
		if err := json.Unmarshal(out, &v); err != nil {
			return nil, fmt.Errorf("calc.sigma: bad result from add: %w", err)
		}
		total = v
	}
	return json.Marshal(total)
}
