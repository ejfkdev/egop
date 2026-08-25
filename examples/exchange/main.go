// 信封翻译表示例(exchange.Register/NewEvent/Decode/DecodeAs):把事件载荷类型
// 登记进翻译表,生产侧 NewEvent 打包成 contract.Event(自动带 SubType + host Origin),
// 订阅方按强类型解回。运行:go run .
package main

import (
	"log"

	"github.com/ejfkdev/egop/exchange"
)

// TaskDone 是事件载荷;NewEvent 传值或指针都会归一为同一 SubType("TaskDone")。
type TaskDone struct {
	ID   string `json:"id"`
	Cost int    `json:"cost"`
}

func main() {
	// 唯一登记:名字与"类型名"一致(NewEvent 自动推导同名 SubType)。
	exchange.Register("TaskDone", TaskDone{})

	// 生产侧:指针载荷也会被解引用归一到 "TaskDone"。
	ev := exchange.NewEvent("task.done", &TaskDone{ID: "job-1", Cost: 42}, "")
	log.Printf("emit: subtype=%s payload=%s", ev.SubType, ev.Payload)

	// 消费侧:强类型解回。
	done, err := exchange.DecodeAs[TaskDone](ev)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("decode: ID=%s Cost=%d", done.ID, done.Cost)

	// 未登记的子类型 → 报错,不透传未知载荷。
	unknown := exchange.NewEvent("unknown", nil, "NotRegistered")
	if _, err := exchange.Decode(unknown); err != nil {
		log.Printf("unregistered subtype rejected: %v", err)
	}
}
