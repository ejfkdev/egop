// 配置 + 函数 schema 校验 + ConfigProvider 权威读回示例。
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

// search 演示 ConfigProvider(权威读回)与字段 Default:max_results 带默认值 10,
// api_key 是 Secret;Config() 返回当前生效配置并对 api_key 脱敏。
type search struct {
	maxResults int
}

func (s *search) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.search", Name: "Search", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{
			{Key: "max_results", Schema: json.RawMessage(`{"type":"integer"}`), Default: json.RawMessage(`10`)},
			{Key: "api_key", Schema: json.RawMessage(`{"type":"string"}`), Secret: true},
		}},
	}
}
func (s *search) ApplyConfig(cfg json.RawMessage) error {
	var v struct {
		MaxResults *int `json:"max_results"`
	}
	_ = json.Unmarshal(cfg, &v)
	if v.MaxResults != nil {
		s.maxResults = *v.MaxResults
	}
	return nil
}
func (s *search) Config() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"max_results": s.maxResults, "api_key": "***"})
	return b
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

	// ---- ConfigProvider 权威读回 + SetConfigField 合并 ----
	// 插件自带默认值(maxResults=10),Config() 返回当前生效配置(api_key 脱敏)。
	s := &search{maxResults: 10}
	if err := h.Register(s); err != nil {
		log.Fatal(err)
	}
	if got, _ := h.EffectiveConfig("demo.search"); true {
		log.Printf("demo.search effective before set = %s", got) // {"max_results":10,"api_key":"***"}
	}
	_ = h.SetConfigField("demo.search", "max_results", json.RawMessage(`25`)) // 合并,保留 api_key
	if got, _ := h.EffectiveConfig("demo.search"); true {
		log.Printf("demo.search effective after patch = %s", got) // {"max_results":25,"api_key":"***"}
	}
	if v, _ := h.GetConfig("demo.search", "max_results"); true {
		log.Printf("GetConfig(max_results) = %s", v) // 25
	}

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
