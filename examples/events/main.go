// 事件总线(发布/订阅)示例:先说后做——声明 event.emit 才能发布、event.listen 才能
// 订阅。事件是一份**固定结构**(contract.Event:Type/SubType/Labels/Payload,框架回填
// Source/Version);订阅按 **EventFilter 匹配**(主题通配/来源/标签),命中即回调。
// egop 用默认内存总线(MemEvents)同步扇出,不持久化。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// producer 声明 event.emit,经 Surface.Publish 发布带标签的完整事件。
type producer struct{ surface contract.Surface }

func (p *producer) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.producer", Name: "Producer", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapEmitsEvents},
			Functions:    []contract.FuncSpec{{Name: "emit"}},
		},
	}
}
func (p *producer) SetSurface(s contract.Surface) { p.surface = s }
func (p *producer) CallFunc(ctx context.Context, _ string, input json.RawMessage) (json.RawMessage, error) {
	var v struct {
		Topic string `json:"topic"`
		Msg   string `json:"msg"`
		Sev   string `json:"sev"`
	}
	_ = json.Unmarshal(input, &v)
	payload, _ := json.Marshal(map[string]string{"msg": v.Msg})
	// 固定结构发布:调用方给 Type(=主题)/SubType/Labels/Payload,框架回填 Source/Version。
	p.surface.Publish(ctx, contract.Event{
		Type:    v.Topic,
		SubType: "chat",
		Payload: payload,
		Labels:  map[string]string{"sev": v.Sev},
	})
	return json.RawMessage(`{"published":true}`), nil
}

// subscriber 声明 event.listen,按过滤条件订阅:主题通配 chat.* + 标签 sev=high。
type subscriber struct{ surface contract.Surface }

func (s *subscriber) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.subscriber", Name: "Subscriber", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapListensEvents}},
	}
}
func (s *subscriber) SetSurface(sur contract.Surface) {
	s.surface = sur
	// 过滤订阅:未设字段 = 不约束;这里 = 主题以 "chat." 开头 且 label sev=high。
	s.surface.SubscribeEventFilter(&contract.EventFilter{
		Type:   "chat.*",
		Labels: map[string]string{"sev": "high"},
	}, func(_ context.Context, topic string, e contract.Event) {
		log.Printf("subscriber(chat.* + sev=high) 收到: topic=%s payload=%s source=%s/%s labels=%v",
			topic, e.Payload, e.Source.Kind, e.Source.ID, e.Labels)
	})
}

func main() {
	ctx := context.Background()
	// 默认事件总线 = 内存 MemEvents(开箱即用;Options.Events 可注入持久化/跨进程实现)。
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	if err := h.Register(&subscriber{}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&producer{}); err != nil {
		log.Fatal(err)
	}

	// 命中(sev=high)→ 触发订阅回调。
	_, _ = h.Call(ctx, "demo.producer", "emit", json.RawMessage(`{"topic":"chat.message","msg":"hello","sev":"high"}`))
	// 未命中(sev=low 不满足标签过滤)→ 静默。
	_, _ = h.Call(ctx, "demo.producer", "emit", json.RawMessage(`{"topic":"chat.message","msg":"ignore","sev":"low"}`))

	_ = h.Close(ctx)
}
