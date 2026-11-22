package wasm

// 线性内存上限是**天花板**不是预留:声明 egop.memory_pages=8192(512MiB)后,
// 实例化后的实际 Memory.Size() 必须远小于上限(Go 运行时按需增长)。
// 这条锁住「调大上限不会立马占用那么多内存」的运维前提。

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"
)

func zipPlugin(t *testing.T, manifest string, wasm []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(manifest))
	w, _ = zw.Create("plugin.wasm")
	_, _ = w.Write(wasm)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMemoryLimitDoesNotPreallocate(t *testing.T) {
	wasm, err := os.ReadFile("testdata/clock.wasm")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		ext   string
		pages uint32
		maxSz uint32
	}{
		{"default", "", DefaultMaxPages, 32 << 20},
		{"high", `,"extensions":{"egop.memory_pages":8192}`, 8192, 32 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := `{"id":"memcap","name":"memcap","version":"1.0.0","provides":{"functions":[{"name":"echo"}]}` + tc.ext + `}`
			p, err := LoadFS(context.Background(), zipPlugin(t, manifest, wasm), "memcap.egop.zip", Options{})
			if err != nil {
				t.Fatalf("LoadFS: %v", err)
			}
			defer p.Close(context.Background())
			if p.memPages != tc.pages {
				t.Fatalf("memPages = %d, want %d", p.memPages, tc.pages)
			}
			if len(p.insts) == 0 || p.insts[0].mod.Memory() == nil {
				t.Fatal("no instance memory")
			}
			sz := p.insts[0].mod.Memory().Size()
			t.Logf("%s: cap=%d pages, committed=%d KiB", tc.name, p.memPages, sz>>10)
			if sz > tc.maxSz {
				t.Fatalf("上限 %d 页却在实例化时提交了 %d KiB(应远小于上限)", tc.pages, sz>>10)
			}
		})
	}
}
