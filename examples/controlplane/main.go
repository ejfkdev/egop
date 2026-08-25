// 控制面(introspection)示例:宿主的元数据反查面——Snapshot 全景快照 / Dependents
// (依赖方) / CapabilityIndex(能力索引) / Functions(函数目录) / Tools(工具面) /
// Plugins / HasPlugin。这些是给装配层、控制面 UI、自检查用的只读查询口。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// base 提供函数 add + 能力 plugin.call。
type base struct{}

func (base) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.base", Name: "Base", Version: "1",
		Provides: contract.Provides{
			Functions:    []contract.FuncSpec{{Name: "add"}},
			Capabilities: []string{contract.CapCallPlugins},
		},
	}
}
func (base) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`42`), nil
}

// tooler 提供工具 probe(tool.provide)。
type tooler struct{}

func (tooler) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.tooler", Name: "Tooler", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapTools}},
	}
}
func (tooler) ToolSpecs() []contract.FuncSpec { return []contract.FuncSpec{{Name: "probe"}} }
func (tooler) Tool(name string) (contract.ToolFunc[struct{}], bool) {
	if name != "probe" {
		return nil, false
	}
	return func(_ context.Context, _ *struct{}, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"hit"`), nil
	}, true
}

func main() {
	ctx := context.Background()
	h := host.New[struct{}](host.Options[struct{}]{Logf: log.Printf})

	if err := h.Register(base{}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&tooler{}); err != nil {
		log.Fatal(err)
	}
	// consumer 声明 DepInit 依赖 demo.base(dependents 反查用)。
	if err := h.Register(&consumer{}); err != nil {
		log.Fatal(err)
	}

	// 全景快照(纯净,不含实例句柄;可序列化给控制面)。
	sb, _ := json.MarshalIndent(h.Snapshot(), "", "  ")
	log.Printf("Snapshot:\n%s", sb)

	// 定向反查。
	log.Printf("dependents(demo.base) = %v", h.Dependents("demo.base"))
	log.Printf("capability[plugin.call] = %v", h.CapabilityIndex()[contract.CapCallPlugins])
	var fns []string
	for _, f := range h.Functions() {
		fns = append(fns, f.PluginID+"."+f.Spec.Name)
	}
	log.Printf("functions = %v", fns)
	var tools []string
	for _, t := range h.Tools() {
		tools = append(tools, t.Info().Name)
	}
	log.Printf("tools = %v", tools)
	log.Printf("has(demo.base)=%v plugins=%d", h.HasPlugin("demo.base"), len(h.Plugins()))

	_ = h.Close(ctx)
}

// consumer 是"依赖方"——只为演示 Dependents 反查(无函数)。
type consumer struct{}

func (consumer) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.consumer", Name: "Consumer", Version: "1",
		Requires: contract.Requires{Deps: []contract.Dependency{{Plugin: "demo.base", Kind: contract.DepInit}}},
	}
}
