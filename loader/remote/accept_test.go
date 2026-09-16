package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

func TestServeStreamOverNetPipe(t *testing.T) {
	// 框架入站 + 插件侧,全部跑在 net.Pipe(纯字节流)——不经过 gRPC/TCP/HTTP,
	// 证明 egop 的远程通道与传输无关。
	h := host.New[any](host.Options[any]{})
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.demo", Name: "Pipe", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "echo"}}},
		},
	}
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	fwStream := BindStream(context.Background(), a) // 框架侧
	plStream := BindStream(context.Background(), b) // 插件侧

	go func() {
		if _, err := AttachStream(context.Background(), plStream, mf, ops); err != nil {
			t.Errorf("attach stream: %v", err)
		}
	}()
	go func() {
		_ = ServeStream(context.Background(), h, fwStream, "", nil)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !h.HasPlugin("pipe.demo") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.HasPlugin("pipe.demo") {
		t.Fatal("plugin never registered over pipe")
	}

	out, err := h.Call(context.Background(), "pipe.demo", "echo", json.RawMessage(`{"x":1}`))
	if err != nil || string(out) != `{"x":1}` {
		t.Fatalf("echo = %s, %v", out, err)
	}
}

func TestDialStreamOverNetPipe(t *testing.T) {
	// 框架出站 + 插件侧,同样只跑 net.Pipe。
	h := host.New[any](host.Options[any]{})
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.out", Name: "Out", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "echo"}}},
		},
	}
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	fwStream := BindStream(context.Background(), a)
	plStream := BindStream(context.Background(), b)

	// 插件侧:作为被拨入的对端,先服务(收框架的 Register,回执)。
	go func() {
		if err := ServePluginStream(context.Background(), plStream, mf, ops); err != nil {
			t.Errorf("serve plugin stream: %v", err)
		}
	}()

	adapter, sess, err := DialStream(context.Background(), h, fwStream, DialOptions{WantID: "pipe.out"})
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	defer sess.Close()
	if err := h.Register(adapter); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !h.HasPlugin("pipe.out") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.HasPlugin("pipe.out") {
		t.Fatal("plugin never registered")
	}

	out, err := h.Call(context.Background(), "pipe.out", "echo", json.RawMessage(`"ok"`))
	if err != nil || string(out) != `"ok"` {
		t.Fatalf("echo = %s, %v", out, err)
	}
}

// TestSameSessionNestedSelfCall 同会话自调(旧 recvLoop 内联派发的死锁形状):
// 插件处理框架调用期间经 HostCall 回程调用**自己**——嵌套请求帧必须有人读。
// 内联派发时本处理阻塞等回复、读循环忙于本处理 → 永久死锁(测试超时);
// 并发派发后嵌套请求照常读入处理,调用链成功。
func TestSameSessionNestedSelfCall(t *testing.T) {
	h := host.New[any](host.Options[any]{})
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.nested", Name: "N", Version: "1",
			Provides: contract.Provides{
				Capabilities: []string{contract.CapCallPlugins},
				Functions:    []contract.FuncSpec{{Name: "outer"}, {Name: "inner"}},
			},
		},
	}
	var plugSess *Session
	// channel 交接会话句柄:attach goroutine 写、主 goroutine 读——无数据竞争。
	sessCh := make(chan *Session, 1)
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			if fname != "outer" {
				return json.RawMessage(`"inner-ok"`), nil
			}
			// 回程经框架调用自己(同一会话):outer 的回复要等本函数返回,
			// 而 inner 请求只能由本会话读循环读——并发派发是唯一活路。
			out, err := plugSess.HostCall(ctx, OpCall, json.RawMessage(
				`{"plugin_id":"pipe.nested","fname":"inner","input":{}}`))
			if err != nil {
				return nil, fmt.Errorf("nested host call: %w", err)
			}
			return out, nil
		},
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	fwStream := BindStream(context.Background(), a)
	plStream := BindStream(context.Background(), b)

	go func() {
		s, err := AttachStream(context.Background(), plStream, mf, ops)
		if err != nil {
			t.Errorf("attach: %v", err)
			return
		}
		sessCh <- s
	}()
	go func() {
		_ = ServeStream(context.Background(), h, fwStream, "", nil)
	}()

	select {
	case plugSess = <-sessCh:
	case <-time.After(2 * time.Second):
		t.Fatal("attach timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !h.HasPlugin("pipe.nested") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.HasPlugin("pipe.nested") {
		t.Fatal("plugin never registered")
	}

	out, err := h.Call(context.Background(), "pipe.nested", "outer", json.RawMessage(`{}`))
	if err != nil || string(out) != `"inner-ok"` {
		t.Fatalf("nested self-call = %s, %v (want inner-ok)", out, err)
	}
}

// TestDispatchConcurrencyLimit 派发配额语义:配额被慢处理器占满时,后续并发
// 请求**立即**回执 busy(不排队不等待);配额释放后新调用照常成功。默认值
// 1024 只有病态洪水才碰(见 DefaultDispatchConcurrency 注释)。
func TestDispatchConcurrencyLimit(t *testing.T) {
	h := host.New[any](host.Options[any]{})
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.lim", Name: "L", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "slow"}}},
		},
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ops := &PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			entered <- struct{}{} // 已进入 handler:配额被占的确定性观测点
			<-release
			return json.RawMessage(`"done"`), nil
		},
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	fwStream := BindStream(context.Background(), a)
	plStream := BindStream(context.Background(), b)

	go func() {
		_ = ServePluginStream(context.Background(), plStream, mf, ops, WithDispatchConcurrency(1))
	}()
	adapter, sess, err := DialStream(context.Background(), h, fwStream, DialOptions{
		WantID: "pipe.lim", DispatchConcurrency: 1,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sess.Close()
	if err := h.Register(adapter); err != nil {
		t.Fatal(err)
	}

	// 第一个慢调用(进入 handler 后占住唯一配额)。
	first := make(chan error, 1)
	go func() {
		_, err := h.Call(context.Background(), "pipe.lim", "slow", json.RawMessage(`{}`))
		first <- err
	}()
	<-entered

	// 第二个并发调用:配额满 → 立即 busy 错误,绝不等待。
	second := make(chan error, 1)
	go func() {
		_, err := h.Call(context.Background(), "pipe.lim", "slow", json.RawMessage(`{}`))
		second <- err
	}()
	select {
	case err := <-second:
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("second concurrent call must fail busy, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second call waited instead of failing busy")
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	// 配额已释放:新调用照常成功。
	out, err := h.Call(context.Background(), "pipe.lim", "slow", json.RawMessage(`{}`))
	if err != nil || string(out) != `"done"` {
		t.Fatalf("after release = %s, %v", out, err)
	}
}
