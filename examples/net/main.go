// 出站网络能力示例:先说后做——插件声明 net.access 才能经 Surface.Net() 发起
// 出站请求(Request:HTTP/HTTPS/SSE/gRPC-Web)与双向消息流(DialStream:WebSocket/
// WebTransport)。egop 不实现传输:本例注入一个自包含的内存后端(无真实网络),
// 生产装配层用 net/http+websocket(桌面)或 fetch/WebSocket/WebTransport(浏览器)。
// egop 还强制"网络协议门":出站目标必须是 http/https/ws/wss 等网络协议,file:// 被拒。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// memNet 是自包含的出站网络后端:Request 返回一段 SSE 风格长响应(带 gRPC trailer),
// DialStream 返回一条回显消息流。真实场景应由装配层注入,而非 egop 实现。
type memNet struct{}

func (memNet) Request(_ context.Context, req contract.Request) (*contract.Response, error) {
	return &contract.Response{
		Status:   200,
		Headers:  map[string]string{"content-type": "text/event-stream"},
		Trailers: map[string]string{"grpc-status": "0"}, // gRPC(-Web) unary 尾部元数据
		Body:     io.NopCloser(strings.NewReader("data: hello\n\n")),
	}, nil
}
func (memNet) DialStream(_ context.Context, url string, _ map[string]string) (contract.Stream, error) {
	// scheme 决定传输:ws/wss=WebSocket、https(HTTP/3)=WebTransport。
	return &memStream{tag: url}, nil
}

type memStream struct{ tag string }

func (s *memStream) Send([]byte) error        { return nil }
func (s *memStream) Recv() ([]byte, error)    { return []byte("stream-msg@" + s.tag), nil }
func (s *memStream) Context() context.Context { return context.Background() }

// fetcher 声明 net.access,经 Surface.Net() 出站。
type fetcher struct{ surface contract.Surface }

func (f *fetcher) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.fetcher", Name: "Fetcher", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapNet},
			Functions:    []contract.FuncSpec{{Name: "fetch"}, {Name: "leak"}},
		},
	}
}
func (f *fetcher) SetSurface(s contract.Surface) { f.surface = s }
func (f *fetcher) CallFunc(ctx context.Context, fname string, _ json.RawMessage) (json.RawMessage, error) {
	net, _ := f.surface.Net()
	if fname == "leak" {
		// 协议门:file:// 不是网络协议,在转交装配实现前即被拒绝。
		if _, err := net.Request(ctx, contract.Request{Method: "GET", URL: "file:///etc/passwd"}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"leaked":true}`), nil
	}
	// 单向请求族:SSE 是"长响应体"——Body 持续读事件流。
	resp, err := net.Request(ctx, contract.Request{Method: "GET", URL: "https://x/sse",
		Headers: map[string]string{"accept": "text/event-stream"}})
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	// 双向消息流族:ws/wss 即 WebSocket,返回字节消息流。
	st, err := net.DialStream(ctx, "wss://x/chat", map[string]string{"origin": "demo"})
	if err != nil {
		return nil, err
	}
	msg, _ := st.Recv()
	return json.Marshal(map[string]string{
		"stream": string(msg),
		"body":   string(body),
		"grpc":   resp.Trailers["grpc-status"],
	})
}

// offline 不声明 net.access,却想取 Net → 被拒(拿不到 Net)。
type offline struct{ surface contract.Surface }

func (o *offline) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.offline", Name: "Offline", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "fetch"}}},
	}
}
func (o *offline) SetSurface(s contract.Surface) { o.surface = s }
func (o *offline) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	if _, ok := o.surface.Net(); !ok {
		return json.RawMessage(`{"got_net":false}`), nil
	}
	return json.RawMessage(`{"got_net":true}`), nil
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf, Net: memNet{}})
	if err := h.Register(&fetcher{}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&offline{}); err != nil {
		log.Fatal(err)
	}

	out, err := h.Call(ctx, "demo.fetcher", "fetch", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fetcher.fetch() = %s", out)

	// 协议门:出站目标是 file:// 本地文件 → 被拒。
	if _, err := h.Call(ctx, "demo.fetcher", "leak", json.RawMessage(`{}`)); err != nil {
		log.Printf("fetcher.leak() rejected: %v", err)
	}

	out, err = h.Call(ctx, "demo.offline", "fetch", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("offline.fetch() = %s", out)

	_ = h.Close(ctx)
}
