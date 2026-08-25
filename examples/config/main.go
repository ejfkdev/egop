// 配置与函数 schema 校验示例:声明了 Config 字段与函数 Input/Output 后,宿主会
// 在下发/调用时校验;函数校验默认开,可用 DisableFuncValidation 全局关闭。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// add 声明了配置字段(整数 level)与函数 add(带 Input/Output JSON Schema)。
type add struct{}

func (add) Meta() contract.Meta {
	return contract.Meta{
		ID: "calc.add", Name: "Add", Version: "1",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{{
				Name:   "add",
				Input:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
				Output: json.RawMessage(`{"type":"integer"}`),
			}},
			Config: []contract.ConfigFieldSpec{{Key: "level", Schema: json.RawMessage(`{"type":"integer"}`)}},
		},
	}
}

func (add) CallFunc(_ context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	_ = json.Unmarshal(input, &in)
	return json.Marshal(in.A + in.B)
}

func (add) ApplyConfig(cfg json.RawMessage) error {
	log.Printf("calc.add config applied: %s", cfg)
	return nil
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf}) // 函数 schema 校验默认开
	if err := h.Register(add{}); err != nil {
		log.Fatal(err)
	}

	// 配置 schema 校验:level 必须是 integer
	if err := h.SetConfig("calc.add", json.RawMessage(`{"level":"high"}`)); err != nil {
		log.Printf("bad config rejected: %v", err)
	}
	_ = h.SetConfig("calc.add", json.RawMessage(`{"level":3}`))

	// 函数入参/返回 schema 校验:缺 b 且 a 是 string → 调用前拒绝
	if _, err := h.Call(ctx, "calc.add", "add", json.RawMessage(`{"a":"x"}`)); err != nil {
		log.Printf("bad input rejected: %v", err)
	}
	out, _ := h.Call(ctx, "calc.add", "add", json.RawMessage(`{"a":20,"b":22}`))
	log.Printf("add(20,22) = %s", out)

	// 关闭校验的对照:同样不合格入参将直通插件,由插件自行解析(v1 插件解析失败)。
	h2 := host.New[any](host.Options[any]{DisableFuncValidation: true})
	if err := h2.Register(add{}); err != nil {
		log.Fatal(err)
	}
	if _, err := h2.Call(ctx, "calc.add", "add", json.RawMessage(`{"a":"x"}`)); err != nil {
		log.Printf("disabled validation => plugin-side error: %v", err)
	}

	_ = h.Close(ctx)
	_ = h2.Close(ctx)
}
