// 传输无关通道示例:插件通道跑在一条**裸 net.Conn(纯字节流)**上——
// 换成 websocket、http 双向体、浏览器消息通道,也只是把它们的双向流 BindStream 一下。
// egop 只负责在这条流上收发 Frame,不建立、也不依赖底层连接。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/loader/remote"
)

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "conn.echo", Name: "Conn Echo", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "echo"}}},
		},
	}
	ops := &remote.PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
	}

	// 框架侧:裸 TCP 监听,每条连接 BindStream 后交给 ServeStream(egop 只收发)。
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = remote.ServeStream(ctx, h, remote.BindStream(ctx, conn), "", log.Printf)
				_ = conn.Close()
			}()
		}
	}()

	// 插件侧:裸 TCP 拨入,BindStream 后 AttachStream 握手注册。
	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		log.Fatal(err)
	}
	go func() { _, _ = remote.AttachStream(ctx, remote.BindStream(ctx, conn), mf, ops) }()

	// 等注册后调用。
	deadline := time.Now().Add(2 * time.Second)
	for !h.HasPlugin("conn.echo") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.HasPlugin("conn.echo") {
		log.Fatal("plugin never registered over raw conn")
	}
	out, err := h.Call(ctx, "conn.echo", "echo", json.RawMessage(`{"hello":"egop"}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("conn.echo.echo() = %s (over raw net.Conn, transport injected)", out)

	_ = conn.Close()
	_ = h.Close(ctx)
}
