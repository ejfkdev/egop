// 全局文件系统能力面自检:fs.read / fs.write 分向门控(fsGuard 单点强制)、
// 未注入 FS 时不可用、未声明能力时拿不到面。
package host

import (
	"os"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// fakeFS 是内存假全局文件系统(测试注入后端;记录写路径供断言)。
type fakeFS struct {
	files   map[string][]byte
	written []string
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	b, ok := f.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeFS) WriteFile(name string, data []byte) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[name] = data
	f.written = append(f.written, name)
	return nil
}

func TestFSCapabilityGating(t *testing.T) {
	back := &fakeFS{files: map[string][]byte{"hello.txt": []byte("world")}}
	h := New[any](Options[any]{FS: back})
	register := func(id string, caps ...string) {
		t.Helper()
		p := &demoPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1",
			Provides: contract.Provides{Capabilities: caps}}}
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	register("blind")                                         // 无能力
	register("reader", contract.CapFSRead)                    // 只读
	register("writer", contract.CapFSWrite)                   // 只写
	register("both", contract.CapFSRead, contract.CapFSWrite) // 读写

	// 无能力:拿不到面。
	bs, _ := h.SurfaceFor("blind")
	if _, ok := bs.FS(); ok {
		t.Fatal("blind (no fs caps) should not get FS")
	}
	// 只读:读放行、写拒绝(先说后做,分向门控)。
	rs, _ := h.SurfaceFor("reader")
	rfs, ok := rs.FS()
	if !ok {
		t.Fatal("reader (fs.read) should get FS")
	}
	if b, err := rfs.ReadFile("hello.txt"); err != nil || string(b) != "world" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if err := rfs.WriteFile("x", []byte("y")); err == nil {
		t.Fatal("reader must not write (fs.write not declared)")
	}
	// 只写:写放行、读拒绝。
	ws, _ := h.SurfaceFor("writer")
	wfs, ok := ws.FS()
	if !ok {
		t.Fatal("writer (fs.write) should get FS")
	}
	if err := wfs.WriteFile("out.txt", []byte("data")); err != nil {
		t.Fatalf("write refused: %v", err)
	}
	if _, err := wfs.ReadFile("hello.txt"); err == nil {
		t.Fatal("writer must not read (fs.read not declared)")
	}
	if len(back.written) != 1 || back.written[0] != "out.txt" {
		t.Fatalf("backend written = %v", back.written)
	}
	// 读写:双向放行。
	boths, _ := h.SurfaceFor("both")
	bfs, ok := boths.FS()
	if !ok {
		t.Fatal("both should get FS")
	}
	if _, err := bfs.ReadFile("hello.txt"); err != nil {
		t.Fatalf("both read: %v", err)
	}
	if err := bfs.WriteFile("both.txt", []byte("z")); err != nil {
		t.Fatalf("both write: %v", err)
	}
}

// TestFSNotInjected 未注入后端:即使声明能力也不可用(nil 语义与 Net/Storage 一致)。
func TestFSNotInjected(t *testing.T) {
	h := New[any](Options[any]{})
	p := &demoPlugin{meta: contract.Meta{ID: "reader", Name: "R", Version: "1",
		Provides: contract.Provides{Capabilities: []string{contract.CapFSRead}}}}
	if err := h.Register(p); err != nil {
		t.Fatal(err)
	}
	s, _ := h.SurfaceFor("reader")
	if _, ok := s.FS(); ok {
		t.Fatal("FS must be unavailable when backend not injected")
	}
}
