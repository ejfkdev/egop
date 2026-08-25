// ScanFS 的只读文件系统注入面:不触碰 os 目录,从 fstest.MapFS 读字节装载。
package wasm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestScanFSFromInjectedFS(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"plugins/demo.egop.wasm":        &fstest.MapFile{Data: mod},
		"plugins/ignored.txt":           &fstest.MapFile{Data: []byte("junk")},
		"plugins/.hidden/bad.egop.wasm": &fstest.MapFile{Data: []byte("junk")},
	}

	plugs, errs := ScanFS(context.Background(), fsys, Options{})
	if len(errs) != 0 {
		t.Fatalf("ScanFS errs = %v", errs)
	}
	if len(plugs) != 1 {
		t.Fatalf("plugs = %d (want 1: ignore non-plugin and hidden dir)", len(plugs))
	}
	defer plugs[0].Close(context.Background())
	if plugs[0].Meta().ID != "wasm.demo" {
		t.Fatalf("id = %s", plugs[0].Meta().ID)
	}
}
