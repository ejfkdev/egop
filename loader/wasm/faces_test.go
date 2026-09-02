// faces.wat 夹具的全链路自检:溯源第三参(6 参新形状)回显、fs_read/fs_write、
// net_request/net_body_read/net_body_close 流式句柄——全部经宿主注入后端走真实
// wazero 实例;另含投递锁忙路径(pushEvent/invokeHook TryLock 跳过)的确定性用例。
package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// facesFS 是全局文件系统注入后端(内存假实现)。
type facesFS struct {
	files   map[string][]byte
	written map[string][]byte
}

func (f *facesFS) ReadFile(name string) ([]byte, error) {
	b, ok := f.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *facesFS) WriteFile(name string, data []byte) error {
	if f.written == nil {
		f.written = map[string][]byte{}
	}
	f.written[name] = data
	return nil
}

// facesNet 是出站网络注入后端:固定 200 + 已知 body(流式读回断言用)。
type facesNet struct{}

func (facesNet) Request(_ context.Context, req contract.Request) (*contract.Response, error) {
	return &contract.Response{
		Status:   200,
		Headers:  map[string]string{"content-type": "text/plain"},
		Trailers: map[string]string{"grpc-status": "0"},
		Body:     io.NopCloser(strings.NewReader("net-body-ok")),
	}, nil
}

func (facesNet) DialStream(context.Context, string, map[string]string) (contract.Stream, error) {
	return nil, io.ErrUnexpectedEOF
}

// callerPlug 是进程内调用方插件(plugin.call):经 Surface.Call 打 wasm.faces,
// 验证调用者 Origin 跨 ABI 第三参透传。
type callerPlug struct {
	surface contract.Surface
}

func (c *callerPlug) Meta() contract.Meta {
	return contract.Meta{ID: "caller.go", Name: "Caller", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapCallPlugins},
			Functions:    []contract.FuncSpec{{Name: "reach"}},
		}}
}

func (c *callerPlug) SetSurface(s contract.Surface) { c.surface = s }

func (c *callerPlug) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return c.surface.Call(ctx, "wasm.faces", "Origin", input)
}

func loadFaces(t *testing.T) *Plugin {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "faces.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadFS(context.Background(), b, "faces.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("load faces: %v", err)
	}
	return p
}

func facesHost(t *testing.T, fsys contract.FS, net contract.Net) (*host.Host[any], *Plugin) {
	t.Helper()
	h := host.New[any](host.Options[any]{FS: fsys, Net: net})
	p := loadFaces(t)
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := h.Register(p); err != nil {
		t.Fatalf("register faces: %v", err)
	}
	return h, p
}

// TestFacesOriginEcho 溯源跨 ABI:宿主直调 → Origin{Kind:host};跨插件经
// Surface.Call → Origin{ID:调用者,Kind:call}。guest 用 6 参 egop_call 第三参收到。
func TestFacesOriginEcho(t *testing.T) {
	h, _ := facesHost(t, nil, nil)

	// 宿主直调:框架注入 Origin{Kind:host, Point:fname}。
	out, err := h.Call(context.Background(), "wasm.faces", "Origin", nil)
	if err != nil {
		t.Fatalf("call Origin: %v", err)
	}
	var o contract.Origin
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("origin echo = %s (%v)", out, err)
	}
	if o.Kind != contract.OriginHost || o.Point != "Origin" || o.At == 0 {
		t.Fatalf("origin = %+v, want kind=host point=Origin", o)
	}

	// 跨插件:caller.go 经 Surface.Call → 被调侧收到调用者身份。
	if err := h.Register(&callerPlug{}); err != nil {
		t.Fatal(err)
	}
	out, err = h.Call(context.Background(), "caller.go", "reach", nil)
	if err != nil {
		t.Fatalf("call reach: %v", err)
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("origin echo (cross) = %s (%v)", out, err)
	}
	if o.ID != "caller.go" || o.Kind != contract.OriginCall {
		t.Fatalf("cross-plugin origin = %+v, want id=caller.go kind=call", o)
	}
}

// TestFacesFSImports fs_read/fs_write 经 wasm ABI 走 Surface.FS 分向门控。
func TestFacesFSImports(t *testing.T) {
	back := &facesFS{files: map[string][]byte{"hello.txt": []byte("world")}}
	h, _ := facesHost(t, back, nil)

	// fs_read("hello.txt"):b64 信封解码后 = 文件原始字节。
	out, err := h.Call(context.Background(), "wasm.faces", "Read", nil)
	if err != nil {
		t.Fatalf("call Read: %v", err)
	}
	if string(out) != "world" {
		t.Fatalf("fs_read = %q, want world", out)
	}
	// fs_write("out.txt","from-guest"):后端可见写入。
	if _, err := h.Call(context.Background(), "wasm.faces", "Write", nil); err != nil {
		t.Fatalf("call Write: %v", err)
	}
	if string(back.written["out.txt"]) != "from-guest" {
		t.Fatalf("backend written = %q", back.written["out.txt"])
	}
}

// TestFacesFSImportsGated 后端未注入:声明了能力也拿不到面,回程干净报错。
func TestFacesFSImportsGated(t *testing.T) {
	h, _ := facesHost(t, nil, nil)
	if _, err := h.Call(context.Background(), "wasm.faces", "Read", nil); err == nil ||
		!strings.Contains(err.Error(), "not available") {
		t.Fatalf("fs_read without backend err = %v", err)
	}
}

// TestFacesNetImports net_request/net_body_read/net_body_close 全链路:
// 请求信封回状态/句柄 → 流式读回 chunk → eof → 显式关闭。
func TestFacesNetImports(t *testing.T) {
	h, _ := facesHost(t, nil, facesNet{})
	call := func(fname string) map[string]any {
		t.Helper()
		out, err := h.Call(context.Background(), "wasm.faces", fname, nil)
		if err != nil {
			t.Fatalf("call %s: %v", fname, err)
		}
		if len(out) == 0 {
			return nil // ok 信封无 result(net_body_close 等空回执)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s result = %s (%v)", fname, out, err)
		}
		return m
	}

	// net_request:status/headers/trailers/body_handle(每插件首句柄 = "1")。
	req := call("Net")
	if req["status"] != float64(200) || req["body_handle"] != "1" {
		t.Fatalf("net_request = %v", req)
	}
	if tr, _ := req["trailers"].(map[string]any); tr["grpc-status"] != "0" {
		t.Fatalf("trailers = %v", req["trailers"])
	}
	// net_body_read:首读 chunk = 完整 body(短响应一次读完)。
	rd := call("Body")
	b64, _ := rd["chunk_b64"].(string)
	if got, _ := base64.StdEncoding.DecodeString(b64); string(got) != "net-body-ok" {
		t.Fatalf("chunk = %q (%s)", got, b64)
	}
	// 再读:eof。
	if eof := call("Body"); eof["eof"] != true {
		t.Fatalf("second read = %v, want eof", eof)
	}
	// net_body_close:幂等收尾;关闭后再读仍是 eof(句柄已遗忘)。
	call("Close")
	if eof := call("Body"); eof["eof"] != true {
		t.Fatalf("read after close = %v, want eof", eof)
	}
}

// TestFacesNetImportsGated 后端未注入:net_request 回程干净报错(能力≠可用)。
func TestFacesNetImportsGated(t *testing.T) {
	h, _ := facesHost(t, nil, nil)
	if _, err := h.Call(context.Background(), "wasm.faces", "Net", nil); err == nil ||
		!strings.Contains(err.Error(), "not available") {
		t.Fatalf("net_request without backend err = %v", err)
	}
}

// TestDeliveryLockBusySkips 投递锁忙路径(决策 #11 的确定性用例):实例锁被占用
// (guest 调用中)时,pushEvent 立即返回(事件丢弃)、invokeHook 返回非阻断
// HookResult 记 Reason——TryLock 语义,绝不阻塞(修复前此形状即永久死锁)。
func TestDeliveryLockBusySkips(t *testing.T) {
	p := mustLoad(t) // demo 夹具:导出 egop_on_event / egop_on_hook
	defer p.Close(context.Background())

	p.mu.Lock()
	// 同一 goroutine 持锁重入(自发布死锁的最小形状):必须立即返回。
	before := time.Now()
	p.pushEvent(context.Background(), "wasm.test.topic", contract.Event{Type: "wasm.test.topic", Payload: json.RawMessage(`{"n":1}`)})
	hr := p.invokeHook(context.Background(), "demo.hook", json.RawMessage(`{}`))
	elapsed := time.Since(before)
	p.mu.Unlock()

	if elapsed > time.Second {
		t.Fatalf("busy delivery took %v (want immediate TryLock skip)", elapsed)
	}
	res, ok := hr.(contract.HookResult)
	if !ok {
		t.Fatalf("invokeHook busy result type = %T", hr)
	}
	if res.Block || !strings.Contains(res.Reason, "busy") {
		t.Fatalf("busy hook result = %+v, want non-blocking with busy Reason", res)
	}
}
