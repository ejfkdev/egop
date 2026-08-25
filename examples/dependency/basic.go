// calc.basic:被依赖的基础插件,提供 add/mul 两个函数。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// basicCalc 是基础算术插件——在本示例里它是被依赖、被跨插件调用的那一方。
type basicCalc struct{}

func (basicCalc) Meta() contract.Meta {
	return contract.Meta{
		ID:      "calc.basic",
		Name:    "Basic Calculator",
		Version: "1.0.0",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{
				// Input/Output 是 JSON Schema(MCP 风格),只作函数目录/工具面的静态描述。
				{
					Name:        "add",
					Description: "输入 {a,b},返回 a+b",
					Input:       json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
					Output:      json.RawMessage(`{"type":"integer"}`),
				},
				{
					Name:        "mul",
					Description: "输入 {a,b},返回 a*b",
					Input:       json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
					Output:      json.RawMessage(`{"type":"integer"}`),
				},
			},
		},
	}
}

// CallFunc 实现 contract.FunctionProvider:宿主据此登记函数目录。
func (basicCalc) CallFunc(_ context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	switch fname {
	case "add":
		return json.Marshal(in.A + in.B)
	case "mul":
		return json.Marshal(in.A * in.B)
	}
	return nil, fmt.Errorf("calc.basic: unknown function %q", fname)
}
