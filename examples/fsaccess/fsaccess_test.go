// examples/fsaccess 冒烟:fs.read/fs.write 分向门控 + 后端策略沙箱。
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

func TestExampleFSAccess(t *testing.T) {
	back := newScopedFS()
	h := host.New[any](host.Options[any]{FS: back})
	for id, caps := range map[string][]string{
		"demo.reader": {contract.CapFSRead},
		"demo.writer": {contract.CapFSWrite},
		"demo.blind":  nil,
	} {
		p := &fsPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1",
			Provides: contract.Provides{Capabilities: caps,
				Functions: []contract.FuncSpec{{Name: "read"}, {Name: "write"}}}}}
		if err := h.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	call := func(id, fn, in string) (string, error) {
		out, err := h.Call(ctx, id, fn, json.RawMessage(in))
		return string(out), err
	}

	// 只读:读放行、写被门拒(先说后做,fsGuard 单点强制)。
	if out, err := call("demo.reader", "read", `{"name":"shared/hello.txt"}`); err != nil || !strings.Contains(out, "world") {
		t.Fatalf("reader read = %s, %v", out, err)
	}
	if _, err := call("demo.reader", "write", `{"name":"shared/x","data":"y"}`); err == nil ||
		!strings.Contains(err.Error(), contract.CapFSWrite) {
		t.Fatalf("reader write must be refused by capability gate: %v", err)
	}

	// 只写:写放行(后端可见)、读被门拒。
	if _, err := call("demo.writer", "write", `{"name":"shared/out.txt","data":"v"}`); err != nil {
		t.Fatalf("writer write = %v", err)
	}
	if b, _ := back.ReadFile("shared/out.txt"); string(b) != "v" {
		t.Fatalf("backend out.txt = %q", b)
	}
	if _, err := call("demo.writer", "read", `{"name":"shared/hello.txt"}`); err == nil ||
		!strings.Contains(err.Error(), contract.CapFSRead) {
		t.Fatalf("writer read must be refused by capability gate: %v", err)
	}

	// 未声明:拿不到面。
	if _, err := call("demo.blind", "read", `{"name":"shared/hello.txt"}`); err == nil {
		t.Fatal("blind plugin must not get FS face")
	}

	// 后端策略沙箱:能力门放行后,实现侧仍挡 shared/ 之外(egop 不越权,策略归实现)。
	if _, err := call("demo.reader", "read", `{"name":"private/secret"}`); err == nil ||
		!strings.Contains(err.Error(), "not visible") {
		t.Fatalf("backend policy must refuse private path: %v", err)
	}
}
