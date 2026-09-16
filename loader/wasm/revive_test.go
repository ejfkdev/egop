package wasm

// revive —— 意外打断自愈:ctx 取消看门狗/trap 把实例打死后,下一次函数调用
// 自动重建(热重启语义):函数恢复、egop_init 重放(事件订阅重挂)、最近配置回放;
// 显式 Close 是终态,不复活。

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestReviveAfterInterrupt(t *testing.T) {
	h, ev := coreHost(t)
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}

	// 基线:函数面可用
	if out, err := p.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err != nil || string(out) != "42" {
		t.Fatalf("baseline add = %s, %v", out, err)
	}
	// 配置下发成功(revive 后应回放)
	if err := p.ApplyConfig(json.RawMessage(`{"k":"v"}`)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// 模拟看门狗打断(与 ctx 取消同路径:关实例 + 置 broken)
	_ = p.insts[0].mod.CloseWithExitCode(context.Background(), 255)
	p.insts[0].broken.Store(true)

	// 下一次调用自动复活
	out, err := p.CallFunc(context.Background(), "add", json.RawMessage(`{}`))
	if err != nil || string(out) != "42" {
		t.Fatalf("revived add = %s, %v", out, err)
	}
	if p.insts[0].broken.Load() {
		t.Fatal("broken must clear after revive")
	}

	// init 重放:事件订阅已重挂(新实例内存 3800 处应写入本次事件)
	evt := contract.Event{Type: "wasm.test.topic", Version: contract.EnvelopeVersion, Payload: json.RawMessage(`{"n":2}`)}
	ev.Dispatch(context.Background(), evt)
	eventJSON, _ := json.Marshal(evt)
	b, ok := p.insts[0].mod.Memory().Read(3800, uint32(len(eventJSON)))
	if !ok || !bytes.Equal(b, eventJSON) {
		t.Fatalf("revived instance missed event: got %q ok=%v want %s", b, ok, eventJSON)
	}

	// 显式 Close = 终态,不再复活
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := p.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("after explicit Close must stay closed, got %v", err)
	}
}

func TestReviveToolFace(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	raw, ok := p.ToolRaw("answer")
	if !ok {
		t.Fatal("answer tool missing")
	}
	// 打断后工具面同样自愈
	_ = p.insts[0].mod.CloseWithExitCode(context.Background(), 255)
	p.insts[0].broken.Store(true)
	s, err := raw(context.Background(), json.RawMessage(`{"run_id":"r1"}`), json.RawMessage(`{"q":1}`))
	if err != nil || s != "42" {
		t.Fatalf("revived tool = %q, %v", s, err)
	}
}

// TestReviveNoHookAccumulation revive 重放 init 不累积 hook 注册:撤销随主实例
// unsubs 清退(旧缺陷:注册挂在 surface effect 栈上,复活一次多一份,
// TriggerHook 结果 1→2)。
func TestReviveNoHookAccumulation(t *testing.T) {
	h, _ := coreHost(t)
	p := mustLoad(t)
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		return len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`)))
	}
	if before := count(); before != 1 {
		t.Fatalf("baseline hook registrations = %d (want 1)", before)
	}
	// 打断 + 复活(下一次调用触发 revive,init 重放重挂 hook)。
	_ = p.insts[0].mod.CloseWithExitCode(context.Background(), 255)
	p.insts[0].broken.Store(true)
	if _, err := p.CallFunc(context.Background(), "add", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if after := count(); after != 1 {
		t.Fatalf("hook registrations accumulated across revive: %d (want 1)", after)
	}
}
