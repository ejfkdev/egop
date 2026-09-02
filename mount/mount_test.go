// 一站式装配测试:目录装载(坏包告警+好包入册)、Watch 热更、远程插件全栈
// (注入传输:StreamDial 出站 + StreamAccept 入站,函数调用 + Close 卸载)。
package mount

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/loader"
	"github.com/ejfkdev/egop/loader/remote"
)

func mountHost(t *testing.T) (loader.HostFace, *host.Host[any]) {
	t.Helper()
	h := host.New[any](host.Options[any]{})
	return h, h
}

func demoModule(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "loader", "wasm", "testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeZip(t *testing.T, dir, name, manifest string, module []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(manifest))
	w, err = zw.Create("plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(module)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const demoManifest = `{"id":"wasm.demo","name":"WASM Demo","version":"1.0.0","provides":{"capabilities":["plugin.call","event.listen","storage.kv","tool.provide"],"functions":[{"name":"add"},{"name":"call"},{"name":"kv"}]}}`

func TestMountLoadsWasmDirsWithWarnings(t *testing.T) {
	hf, h := mountHost(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.egop.zip"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeZip(t, dir, "good.egop.zip", demoManifest, demoModule(t))

	rt, warns, err := Mount(context.Background(), hf, Sources{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(warns) != 1 || !h.HasPlugin("wasm.demo") {
		t.Fatalf("warns=%v plugins=%v", warns, h.Plugins())
	}
	out, err := h.Call(context.Background(), "wasm.demo", "add", json.RawMessage(`{}`))
	if err != nil || string(out) != "42" {
		t.Fatalf("add = %s, %v", out, err)
	}
}

func TestMountLoadsFromInjectedFS(t *testing.T) {
	// 一站式装配的 FS 注入面:Sources.FS 非 nil 时 Dirs 是 FS 内的根目录。
	hf, h := mountHost(t)
	fsys := fstest.MapFS{
		"plugins/demo.egop.wasm": &fstest.MapFile{Data: demoModule(t)},
	}
	rt, warns, err := Mount(context.Background(), hf, Sources{Dirs: []string{"plugins"}, FS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(warns) != 0 || !h.HasPlugin("wasm.demo") {
		t.Fatalf("warns=%v plugins=%v", warns, h.Plugins())
	}
}

func TestMountLoadsOutOfOrderDependency(t *testing.T) {
	// 依赖方 a.consumer 依赖 z.base,但文件名字典序 a 先被扫到(乱序):
	// 首装应拍到稳定,把后落地件补载;且"先失败后成功"的瞬态告警应被抑制。
	hf, h := mountHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "a.consumer.egop.zip", `{"id":"a.consumer","name":"C","version":"1","requires":{"deps":[{"plugin":"z.base","kind":"init"}]}}`, demoModule(t))
	writeZip(t, dir, "z.base.egop.zip", `{"id":"z.base","name":"Z","version":"1"}`, demoModule(t))

	rt, warns, err := Mount(context.Background(), hf, Sources{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if !h.HasPlugin("z.base") || !h.HasPlugin("a.consumer") {
		t.Fatalf("both should load: warns=%v plugins=%v", warns, h.Plugins())
	}
	if len(warns) != 0 {
		t.Fatalf("no warnings expected (transient dep-order failure suppressed): %v", warns)
	}
}

func TestMountWatchReloads(t *testing.T) {
	hf, h := mountHost(t)
	dir := t.TempDir()
	rt, _, err := Mount(context.Background(), hf, Sources{
		Dirs: []string{dir}, Watch: true, Interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if h.HasPlugin("wasm.demo") {
		t.Fatal("nothing should be loaded yet")
	}
	// 热更:写入插件包 → 监视循环装载
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))
	waitCond(t, 2*time.Second, func() bool { return h.HasPlugin("wasm.demo") })
	// 删除 → 卸载
	if err := os.Remove(filepath.Join(dir, "demo.egop.zip")); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 2*time.Second, func() bool { return !h.HasPlugin("wasm.demo") })
}

func TestMountStreamAcceptInjected(t *testing.T) {
	// StreamAccept 注入:入站不走 gRPC,外部 transport 给一段 net.Pipe 流即可;
	// 插件侧(主动方)跑 AttachStream 拨入。
	hf, h := mountHost(t)
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.in", Name: "PipeIn", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "echo"}}},
		},
	}
	ops := &remote.PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
	}

	served := false
	accept := func(ctx context.Context) (remote.Stream, error) {
		// accept 循环单 goroutine 顺序调用:第一回给一条真实连接,之后阻塞至取消,
		// 避免空转(真实 transport 的 Accept 也是阻塞的)。
		if !served {
			served = true
			a, b := net.Pipe()
			go func() {
				sess, err := remote.AttachStream(ctx, remote.BindStream(ctx, b), mf, ops)
				if err != nil {
					_ = b.Close()
					return
				}
				<-sess.Done() // 插件会话存活至框架断开(不提前关流,以免立即触发卸载)
				_ = b.Close()
			}()
			return remote.BindStream(ctx, a), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	rt, warns, err := Mount(context.Background(), hf, Sources{StreamAccept: accept})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	waitCond(t, 2*time.Second, func() bool { return h.HasPlugin("pipe.in") })
	out, err := h.Call(context.Background(), "pipe.in", "echo", json.RawMessage(`"bytestream"`))
	if err != nil || string(out) != `"bytestream"` {
		t.Fatalf("echo = %s, %v", out, err)
	}
}

func waitCond(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// eofStream 是立即返回 EOF 的出站流:复现握手失败路径的会话收尾回归。
type eofStream struct{ ctx context.Context }

func (e eofStream) Send([]byte) error        { return nil }
func (e eofStream) Recv() ([]byte, error)    { return nil, io.EOF }
func (e eofStream) Context() context.Context { return e.ctx }
func (e eofStream) Close() error             { return nil }

func TestMountDialHandshakeFailureNoPanic(t *testing.T) {
	// 回归:握手失败(对端立即断流)的出站远程插件曾触发 sess.Close() 的 nil 解引用
	// panic(Mount 整体崩溃)。此处应返回装配错误,而非 panic。
	hf, _ := mountHost(t)
	dial := func(ctx context.Context, addr string) (remote.Stream, error) {
		return eofStream{ctx: ctx}, nil
	}
	_, _, err := Mount(context.Background(), hf, Sources{
		Remote:     []RemoteSpec{{ID: "bad.out", Addr: "custom://bad.out"}},
		StreamDial: dial,
	})
	if err == nil {
		t.Fatal("expected handshake failure error, got nil")
	}
}

func TestMountStreamDialInjected(t *testing.T) {
	// StreamDial 注入:出站不走 gRPC,外部 transport 给一条 net.Pipe 流即可。
	hf, h := mountHost(t)
	mf := contract.Manifest{
		Meta: contract.Meta{
			ID: "pipe.out", Name: "PipeOut", Version: "1",
			Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "echo"}}},
		},
	}
	ops := &remote.PluginOps{
		CallFunc: func(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
			return input, nil
		},
	}

	dial := func(ctx context.Context, addr string) (remote.Stream, error) {
		a, b := net.Pipe()
		go func() {
			_ = remote.ServePluginStream(ctx, remote.BindStream(ctx, b), mf, ops)
			_ = b.Close()
		}()
		return remote.BindStream(ctx, a), nil
	}

	rt, warns, err := Mount(context.Background(), hf, Sources{
		Remote:     []RemoteSpec{{ID: "pipe.out", Addr: "custom://pipe.out"}},
		StreamDial: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	waitCond(t, 2*time.Second, func() bool { return h.HasPlugin("pipe.out") })
	out, err := h.Call(context.Background(), "pipe.out", "echo", json.RawMessage(`"bytestream"`))
	if err != nil || string(out) != `"bytestream"` {
		t.Fatalf("echo = %s, %v", out, err)
	}
}

// TestMountExtraSuffixes 品牌 zip 后缀经 Sources.ExtraSuffixes 注入,贯通
// mount → autoload → wasm 三层(首装即按注入后缀发现装载)。
func TestMountExtraSuffixes(t *testing.T) {
	hf, h := mountHost(t)
	dir := t.TempDir()
	writeZip(t, dir, "demo.brand.zip", demoManifest, demoModule(t))

	// 未注入:该后缀被忽略,不装载。
	rt, warns, err := Mount(context.Background(), hf, Sources{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 || h.HasPlugin("wasm.demo") {
		t.Fatalf("un-injected suffix loaded: warns=%v plugins=%v", warns, h.Plugins())
	}
	rt.Close()

	// 注入:首装即发现。
	rt2, _, err := Mount(context.Background(), hf, Sources{Dirs: []string{dir}, ExtraSuffixes: []string{".brand.zip"}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if !h.HasPlugin("wasm.demo") {
		t.Fatalf("injected suffix not loaded: plugins=%v", h.Plugins())
	}
}

// TestMountFailureFullUnwind 装配失败全清:目录阶段已进册的插件被反注册
// (Watcher.Unload),宿主回到装配前状态——出站远程缺 StreamDial 即失败。
func TestMountFailureFullUnwind(t *testing.T) {
	hf, h := mountHost(t)
	// 预置一个装配前就在册的插件:全清不得误伤它(只清本次目录装载的)。
	pre := &prePlug{}
	if err := h.Register(pre); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeZip(t, dir, "demo.egop.zip", demoManifest, demoModule(t))

	_, _, err := Mount(context.Background(), hf, Sources{
		Dirs:   []string{dir},
		Remote: []RemoteSpec{{ID: "remote.demo", Addr: "x"}}, // 无 StreamDial → 失败
	})
	if err == nil {
		t.Fatal("mount without StreamDial must fail")
	}
	if h.HasPlugin("wasm.demo") {
		t.Fatalf("dir-loaded plugin must be unregistered on failure: %v", h.Plugins())
	}
	if !h.HasPlugin("pre.existing") {
		t.Fatal("pre-existing plugin must survive mount failure cleanup")
	}
}

// prePlug 是装配前已在册的最小插件(全清边界断言用)。
type prePlug struct{}

func (prePlug) Meta() contract.Meta {
	return contract.Meta{ID: "pre.existing", Name: "P", Version: "1"}
}
