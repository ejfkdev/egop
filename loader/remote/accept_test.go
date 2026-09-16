package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
