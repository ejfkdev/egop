package wasm

import (
	"context"
	"encoding/json"
	"errors"
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

func TestPoolHookPinnedToPrimary(t *testing.T) {
	h, _ := coreHost(t)
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		return len(h.TriggerHook(context.Background(), "demo.hook", json.RawMessage(`{}`)))
	}
	if before := count(); before != 1 {
		t.Fatalf("baseline hook registrations = %d, want 1", before)
	}
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if after := count(); after != 1 {
		t.Fatalf("secondary instance registered a duplicate hook callback: got %d, want 1", after)
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

// TestGuestDeathSelfHeal guest 自发死亡(module closed,非 ctx 看门狗路径)不再
// 永久僵死插件:死亡当次调用如实报错(fail-closed)且实例置 broken,下一次调用
// revive 重建自愈。此前 callCore 只在看门狗中断路径置 broken——guest 自发
// exit(如 exit_code(4))/trap 后实例永不自愈,插件僵死到宿主重启
// (eha longrun 生产事故实锤,七十五阶修)。
func TestGuestDeathSelfHeal(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	if _, err := p.CallFunc(context.Background(), "add", nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	i := p.insts[0]
	i.mu.Lock()
	_ = i.mod.CloseWithExitCode(context.Background(), 4)
	i.mu.Unlock()
	// 死亡当次:调用如实报错,且置 broken(revive 前提)。
	if _, err := p.CallFunc(context.Background(), "add", nil); err == nil {
		t.Fatal("死实例调用应报错(fail-closed)")
	}
	if !i.broken.Load() {
		t.Fatal("非 ctx 取消的调用错误应置 broken(否则永不 revive)")
	}
	// 下一次调用:revive 重建,插件自愈。
	if _, err := p.CallFunc(context.Background(), "add", nil); err != nil {
		t.Fatalf("revive 应自愈: %v", err)
	}
}

// TestPoolAcquireFIFOFairness 公平性回归锁(2026-10-15 eha 跑分实锤:纯
// TryLock 自旋无 FIFO,长阻塞轮次持实例时新到者无限插队,先到等待者饥饿
// 35+ 分钟)。语义:排队者按入队序获得交接;释放优先递队首而非新到者。
func TestPoolAcquireFIFOFairness(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	ctx := context.Background()
	held, err := p.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 4)
	waiter := func(name string, ready chan<- string) {
		ready <- name
		i, err := p.acquire(ctx)
		if err != nil {
			t.Errorf("%s acquire: %v", name, err)
			order <- name + ":err"
			return
		}
		order <- name
		time.Sleep(5 * time.Millisecond)
		p.release(i)
	}
	// B 先排队,C 后排队;释放后必须 B 先拿到(旧实现下 C 可插队)。
	rb := make(chan string, 1)
	rc := make(chan string, 1)
	go waiter("B", rb)
	<-rb
	time.Sleep(30 * time.Millisecond) // 确保 B 已入队
	go waiter("C", rc)
	<-rc
	time.Sleep(30 * time.Millisecond) // 确保 C 已入队(B 在前)
	// 交接开始:D 在释放瞬间抢跑也排不到 B/C 前面。
	go func() {
		time.Sleep(5 * time.Millisecond)
		i, err := p.acquire(ctx)
		if err == nil {
			order <- "D"
			p.release(i)
		}
	}()
	p.release(held)
	got := []string{<-order, <-order, <-order}
	if got[0] != "B" || got[1] != "C" {
		t.Fatalf("FIFO 违约: %v (期望 B,C 先于 D)", got)
	}
}

func TestPoolCanceledHandoffReleasesToNextWaiter(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	held, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan *inst, 1)
	secondErr := make(chan error, 1)
	go func() {
		i, err := p.acquire(context.Background())
		if err != nil {
			secondErr <- err
			return
		}
		secondDone <- i
	}()
	deadline := time.Now().Add(time.Second)
	for {
		p.waitMu.Lock()
		queued := len(p.waiters)
		p.waitMu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second waiter did not enqueue")
		}
		time.Sleep(time.Millisecond)
	}

	// Model the cancellation race deterministically: the canceled first waiter
	// has already been popped and handed the locked instance, while a real
	// second waiter remains queued behind it.
	first := make(chan *inst, 1)
	p.waitMu.Lock()
	p.waiters = append([]chan *inst{first}, p.waiters...)
	p.waiters = p.waiters[1:]
	first <- held
	p.waitMu.Unlock()
	p.cancelWaiter(first)

	select {
	case i := <-secondDone:
		if i != held {
			t.Fatal("remaining waiter received a different instance")
		}
		p.release(i)
	case err := <-secondErr:
		t.Fatalf("remaining waiter: %v", err)
	case <-time.After(time.Second):
		t.Fatal("canceled FIFO winner stranded the remaining waiter")
	}
}

// TestPoolCloseWakesWaiters Close 唤醒排队者(收关闭错误,不挂死)。
func TestPoolCloseWakesWaiters(t *testing.T) {
	p := mustLoad(t)
	ctx := context.Background()
	held, err := p.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		_, err := p.acquire(context.Background())
		res <- err
	}()
	time.Sleep(30 * time.Millisecond) // 等待者已入队
	go p.Close(context.Background())
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("排队者应收关闭错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 未唤醒排队者(挂死)")
	}
	_ = held
}

// TestToolSpecsNonBlockingWhenBusy 元数据查询不得排在长调用后面(2026-10-31):
// 实例被整轮占用时 ToolSpecs 必须立即回快照/清单,而非烧满 10s 超时(旧实现
// 每次 eha agent 启动会被 LLM/react 忙插件各拖 10s)。
func TestToolSpecsNonBlockingWhenBusy(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	// 预热:空闲时查询(清单快照,无 export 时回 manifest)。
	if specs := p.ToolSpecs(); len(specs) == 0 || specs[0].Name != "answer" {
		t.Fatalf("idle specs = %+v", specs)
	}
	ctx := context.Background()
	i, err := p.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	specs := p.ToolSpecs()
	elapsed := time.Since(start)
	p.release(i)
	if len(specs) == 0 || specs[0].Name != "answer" {
		t.Fatalf("busy specs = %+v", specs)
	}
	if elapsed > time.Second {
		t.Fatalf("ToolSpecs blocked %v while instance busy (must be non-blocking)", elapsed)
	}
	p.waitMu.Lock()
	nWait := len(p.waiters)
	p.waitMu.Unlock()
	if nWait != 0 {
		t.Fatalf("ToolSpecs enqueued %d waiter(s); metadata must never queue", nWait)
	}
}

func TestPoolBusyWhenAnyInstanceActive(t *testing.T) {
	p := mustLoad(t)
	defer p.Close(context.Background())
	if err := p.growPool(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	i, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	busy, err := p.Busy()
	p.release(i)
	if err != nil {
		t.Fatal(err)
	}
	if !busy {
		t.Fatal("one active instance must make the pool busy even when another is idle")
	}
}

func TestCloseDeadlineInterruptsBlockedCall(t *testing.T) {
	module, err := os.ReadFile(filepath.Join("testdata", "blocked.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	p, err := LoadFS(context.Background(), buildZip(t,
		`{"id":"wasm.blocked","name":"Blocked","version":"1","provides":{"functions":[{"name":"block"}]}}`, module),
		"blocked.egop.zip", Options{LogFn: func(_, msg string) {
			if msg == "started" {
				select {
				case started <- struct{}{}:
				default:
				}
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.runtime != nil {
			_ = p.runtime.CloseWithExitCode(context.Background(), 255)
		}
		_ = p.Close(context.Background())
	})

	callDone := make(chan error, 1)
	go func() {
		_, err := p.CallFunc(context.Background(), "block", nil)
		callDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked call did not enter the guest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close(ctx) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned an error after interrupting the call: %v", err)
		}
	case <-time.After(time.Second):
		_ = p.runtime.CloseWithExitCode(context.Background(), 255)
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("Close remained blocked on the active instance past its context deadline")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("forcibly interrupted call must return an error")
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted call did not return")
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close must be safe: %v", err)
	}
}

func TestCloseDeadlineBoundsUninterruptibleInstance(t *testing.T) {
	p := mustLoad(t)
	i, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close(ctx) }()
	var closeErr error
	timedOut := false
	select {
	case closeErr = <-closeDone:
	case <-time.After(time.Second):
		timedOut = true
	}
	elapsed := time.Since(started)
	p.release(i)
	if timedOut {
		t.Fatal("Close did not observe its context while the instance lock stayed held")
	}
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline", closeErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close returned after %v, well past its 40ms deadline", elapsed)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close must be safe: %v", err)
	}
}
