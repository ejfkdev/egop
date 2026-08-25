// OS 便捷装载面(LoadFile/ScanDir)与 .egop.zip 静态资源拆包的测试:
// ScanFS 注入缝已有覆盖,这里补两条 os 直读的便捷入口 + assets 提取。
package wasm

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileAndScanDir(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "demo.egop.wasm")
	if err := os.WriteFile(p, mod, 0o644); err != nil {
		t.Fatal(err)
	}

	// LoadFile:按 OS 路径加载单文件(便捷面;读路径本体应走 FS 注入缝,此为桌面便利)。
	pl, err := LoadFile(context.Background(), p, Options{})
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if pl.Meta().ID != "wasm.demo" {
		t.Fatalf("LoadFile id = %s", pl.Meta().ID)
	}
	_ = pl.Close(context.Background())

	// ScanDir:目录遍历(=ScanFS(os.DirFS(dir)));忽略非插件后缀。
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugs, errs := ScanDir(context.Background(), dir, Options{})
	if len(errs) != 0 || len(plugs) != 1 {
		t.Fatalf("ScanDir plugs=%d errs=%v", len(plugs), errs)
	}
	_ = plugs[0].Close(context.Background())
}

func TestParseZipAssets(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"id":"wasm.demo","name":"D","version":"1"}`))
	w, _ = zw.Create("plugin.wasm")
	_, _ = w.Write(mod)
	w, _ = zw.Create("assets/foo.txt")
	_, _ = w.Write([]byte("hello asset"))
	w, _ = zw.Create("assets/sub/bar.bin")
	_, _ = w.Write([]byte{0x01, 0x02})
	_ = zw.Close()

	mf, wasmBytes, assets, err := parseZip(buf.Bytes())
	if err != nil {
		t.Fatalf("parseZip: %v", err)
	}
	if mf.ID != "wasm.demo" {
		t.Fatalf("manifest id = %s", mf.ID)
	}
	if len(wasmBytes) != len(mod) {
		t.Fatalf("wasmBytes len = %d", len(wasmBytes))
	}
	if string(assets["foo.txt"]) != "hello asset" {
		t.Fatalf("assets[foo.txt] = %q", assets["foo.txt"])
	}
	if got := assets["sub/bar.bin"]; !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("assets[sub/bar.bin] = %v", got)
	}
	if _, ok := assets["missing"]; ok {
		t.Fatal("assets should not include missing")
	}
}
