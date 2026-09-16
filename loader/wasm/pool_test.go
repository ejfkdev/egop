package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// countingEvents 包真 MemEvents:计数每次 Dispatch 实际扇出的回调数
// (即插件注册了多少个互不相同的订阅)。
type countingEvents struct {
	inner host.Events
	hits  atomic.Int64
}

func (c *countingEvents) Subscribe(f *contract.EventFilter, fn func(context.Context, contract.Event)) func() {
	return c.inner.Subscribe(f, func(ctx context.Context, ev contract.Event) {
		c.hits.Add(1)
		fn(ctx, ev)
	})
}
func (c *countingEvents) Dispatch(ctx context.Context, ev contract.Event) { c.inner.Dispatch(ctx, ev) }
func (c *countingEvents) EnsureTopic(t string)                            { c.inner.EnsureTopic(t) }

var _ host.Events = (*countingEvents)(nil)

// TestPoolNestedAcquire 实例池核心语义:池≥2 时第二取者(嵌套或并发)立即拿到
// 另一实例;池耗尽时并发取者等 ctx 超时返回错误,不阻塞重入者。
func TestPoolNestedAcquire(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	i1, err := p.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 并发第二取:必须立即拿到另一实例(死锁=此处挂死)。
	done := make(chan *inst, 1)
	go func() {
		i2, err := p.acquire(ctx)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			done <- nil
			return
		}
		done <- i2
	}()
	i2 := <-done
	if i2 == nil {
		t.Fatal("second acquire failed")
	}
	if i1 == i2 {
		t.Fatal("second acquire returned same instance")
	}
	// 池耗尽:第三取(两实例被持)应超时错误,不无限阻塞。
	sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	before := time.Now()
	if _, err := p.acquire(sctx); err == nil {
		t.Fatal("third acquire should fail busy")
	}
	if elapsed := time.Since(before); elapsed > 2*time.Second {
		t.Fatalf("busy acquire blocked %v (must not block nested goroutine)", elapsed)
	}
	p.release(i2)
	p.release(i1)
}

// TestAcquireSameStackNestedBusy 同调用栈嵌套(外层持有 + ctx 重入标记):
// 池=1 立即返回 busy 错误(等待即自死锁),绝不依赖 ctx 取消;池≥2 取得第二实例。
func TestAcquireSameStackNestedBusy(t *testing.T) {
	p := mustLoad(t) // 池=1
	defer p.Close(context.Background())
	ctx := withHeld(context.Background(), p) // 模拟外层持有后经 wazero 透传的 ctx
	i1, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.release(i1)
	before := time.Now()
	_, err = p.acquire(ctx) // 同栈嵌套:必须立即 busy
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("nested same-stack acquire must fail busy immediately, got %v", err)
	}
	if elapsed := time.Since(before); elapsed > 500*time.Millisecond {
		t.Fatalf("nested busy took %v (want immediate)", elapsed)
	}
	// 池≥2:同栈嵌套取第二实例,不报 busy。
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	i2, err := p.acquire(ctx)
	if err != nil {
		t.Fatalf("nested with pool=2: %v", err)
	}
	if i2 == i1 {
		t.Fatal("nested acquire must take the second instance")
	}
	p.release(i2)
}

// TestReleaseAfterCloseNoHang Close 与在册调用并发:release(仅解锁)必须返回
// ——旧信号量制在 Close 置 nil 后 <-nil 永挂,Host.Call 调用方永不返回。
func TestReleaseAfterCloseNoHang(t *testing.T) {
	p := mustLoad(t)
	i, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = p.Close(context.Background()) // 等 i.mu 释放后收尾
		close(closed)
	}()
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond) // 让 Close 先摘表
		p.release(i)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CONFIRMED regression: release hangs after Close")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish after release")
	}
}

// TestPoolEventPinnedToPrimary 池>1 时事件语义:恰好一个订阅(挂主实例)、
// 恰好一次投递、只落主实例内存——次实例不参与观察面。
func TestPoolEventPinnedToPrimary(t *testing.T) {
	bus := &countingEvents{inner: host.NewMemEvents()}
	h := host.New[any](host.Options[any]{Events: bus, Storage: memSt{}, Settings: settingsSource{}})
	p := mustLoad(t)
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	evt := contract.Event{Type: "wasm.test.topic", Version: contract.EnvelopeVersion, Payload: json.RawMessage(`{"n":9}`)}
	bus.Dispatch(context.Background(), evt)
	if n := bus.hits.Load(); n != 1 {
		t.Fatalf("event fanned out to %d subscription callbacks (want exactly 1: primary only)", n)
	}
	eventJSON, _ := json.Marshal(evt)
	b0, ok0 := p.insts[0].mod.Memory().Read(3800, uint32(len(eventJSON)))
	b1, ok1 := p.insts[1].mod.Memory().Read(3800, uint32(len(eventJSON)))
	if !ok0 || string(b0) != string(eventJSON) {
		t.Fatalf("primary instance must receive the event: %q ok=%v", b0, ok0)
	}
	if ok1 && string(b1) == string(eventJSON) {
		t.Fatal("secondary instance must NOT receive the event (pinned to primary)")
	}
}

// TestNestedSelfCallEndToEnd 端到端嵌套自调用:guest 经宿主 call 注入回调
// **本插件**(同 goroutine 重入)。验证两件事:① ctx 重入标记跨 wazero 透传
// (不透传则嵌套 acquire 永挂、本测试超时);② 池=1 立即 busy 回落、池=2 取
// 第二实例调用链成功。夹具 testdata/nested.wat。
func TestNestedSelfCallEndToEnd(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "nested.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	h := host.New[any](host.Options[any]{})
	p, err := LoadFS(context.Background(), b, "nested.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	// 池=1:嵌套自调用必须立即返回 busy 错误(等待即同栈自死锁)。
	before := time.Now()
	_, err = h.Call(context.Background(), "wasm.nested", "self", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("nested self-call (pool=1) must fail busy, got %v", err)
	}
	if elapsed := time.Since(before); elapsed > 5*time.Second {
		t.Fatalf("nested busy took %v (want immediate; ctx marker not propagated?)", elapsed)
	}
	// 池=2:嵌套取第二实例,调用链成功(42 = guest deep 的静态返回)。
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	out, err := h.Call(context.Background(), "wasm.nested", "self", json.RawMessage(`{}`))
	if err != nil || string(out) != "42" {
		t.Fatalf("nested self-call (pool=2) = %s, %v", out, err)
	}
}

// TestPoolConcurrentCalls 池=2 时两个并发 CallFunc 各占一实例(不串行排队失败)。
func TestPoolConcurrentCalls(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := p.CallFunc(context.Background(), "add", nil)
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent CallFunc: %v", err)
		}
	}
}
