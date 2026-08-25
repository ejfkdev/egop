package remote

import (
	"context"
	"net"
	"testing"
)

func TestBindStreamRoundTrip(t *testing.T) {
	// 用 net.Pipe 模拟任意字节流:egop 侧 BindStream 收发 JSON 帧,与 gRPC/TCP 无关——
	// 换成 http/websocket/浏览器通道也只是换一个提供读写字节的实现。
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	sa := BindStream(context.Background(), a)
	sb := BindStream(context.Background(), b)

	go func() { _ = sendFrame(sa, &Frame{Id: 1, Reply: true}) }()
	f, err := recvFrame(sb)
	if err != nil || f.Id != 1 || !f.Reply {
		t.Fatalf("a->b recv = %+v, %v", f, err)
	}

	go func() { _ = sendFrame(sb, &Frame{Id: 2}) }()
	f, err = recvFrame(sa)
	if err != nil || f.Id != 2 {
		t.Fatalf("b->a recv = %+v, %v", f, err)
	}
}
