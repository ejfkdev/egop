// 工具示例:声明 tool.provide 能力,把一个"返回字符串 + 带上下文"的可调用体
// 暴露到宿主工具面。运行:go run .
//
// 与普通函数唯一的差别在**执行形态**(返回字符串、多一个不透明上下文),
// spec 共用 contract.FuncSpec——区别在注册面(声明 tool.provide)。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// greeter 提供工具面(greet)。
type greeter struct{}

func (greeter) Meta() contract.Meta {
	return contract.Meta{
		ID:      "tool.greeter",
		Name:    "Greeter",
		Version: "1.0.0",
		Provides: contract.Provides{
			// 声明 tool.provide 才会被收进宿主工具面。
			Capabilities: []string{contract.CapTools},
		},
	}
}

// ToolSpecs 声明工具清单(与函数共用 FuncSpec)。
func (greeter) ToolSpecs() []contract.FuncSpec {
	return []contract.FuncSpec{{
		Name:        "greet",
		Description: "按名字问候",
		Input:       json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		Output:      json.RawMessage(`{"type":"string"}`),
	}}
}

// Tool 按名返回工具执行闭包(返回 JSON;宿主 Run 时 string 化)。
func (greeter) Tool(name string) (contract.ToolFunc[struct{}], bool) {
	if name != "greet" {
		return nil, false
	}
	return func(ctx context.Context, tctx *struct{}, input json.RawMessage) (json.RawMessage, error) {
		var in struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(input, &in)
		return json.RawMessage(fmt.Sprintf(`"hello, %s"`, in.Name)), nil
	}, true
}

func main() {
	ctx := context.Background()
	h := host.New[struct{}](host.Options[struct{}]{Logf: log.Printf})
	if err := h.Register(&greeter{}); err != nil {
		log.Fatal(err)
	}

	tools := h.Tools() // 只收声明了 tool.provide 的插件
	log.Printf("tool 面已就位 %d 个", len(tools))
	for _, t := range tools {
		out, err := t.Run(ctx, &struct{}{}, json.RawMessage(`{"name":"egop"}`))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("%s(%s) = %s", t.Info().Name, t.Info().Description, out)
	}

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("host closed")
}
