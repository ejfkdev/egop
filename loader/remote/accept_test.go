package remote

import (
	"context"
	"encoding/json"
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
