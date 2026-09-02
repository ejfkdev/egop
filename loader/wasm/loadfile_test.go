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

	mf, wasmBytes, assets, err := parseZip(buf.Bytes(), Options{})
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

// TestPluginAssetsExport 验证装好的 Plugin 经 Assets() 暴露包内静态资源:
// zip 形态有资产、裸 wasm 形态为空表;返回副本不污染插件内部。
func TestPluginAssetsExport(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}

	// zip 形态:assets/ 两个文件。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"id":"wasm.demo","name":"D","version":"1"}`))
	w, _ = zw.Create("plugin.wasm")
	_, _ = w.Write(mod)
	w, _ = zw.Create("assets/pane.js")
	_, _ = w.Write([]byte("console.log('hi')"))
	w, _ = zw.Create("assets/sub/style.css")
	_, _ = w.Write([]byte("body{}"))
	_ = zw.Close()

	p, err := LoadFS(context.Background(), buf.Bytes(), "demo.egop.zip", Options{})
	if err != nil {
		t.Fatalf("LoadFS zip: %v", err)
	}
	defer p.Close(context.Background())
	got := p.Assets()
	if string(got["pane.js"]) != "console.log('hi')" {
		t.Fatalf("Assets[pane.js] = %q", got["pane.js"])
	}
	if string(got["sub/style.css"]) != "body{}" {
		t.Fatalf("Assets[sub/style.css] = %q", got["sub/style.css"])
	}
	// 副本语义:改返回 map 不影响插件内部。
	delete(got, "pane.js")
	if _, ok := p.Assets()["pane.js"]; !ok {
		t.Fatal("Assets() must return a copy; plugin table was mutated")
	}

	// 裸 wasm 形态:空表而非 nil。
	p2, err := LoadFS(context.Background(), mod, "demo.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("LoadFS wasm: %v", err)
	}
	defer p2.Close(context.Background())
	if a := p2.Assets(); a == nil || len(a) != 0 {
		t.Fatalf("bare wasm Assets() = %v (want empty non-nil)", a)
	}
}

func TestConfigExportReadback(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "config.egop.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadFS(context.Background(), b, "config.egop.wasm", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	if got := p.Config(); string(got) != `{"api_url":"https://default","max_results":10}` {
		t.Fatalf("Config() = %s", got)
	}
}

func TestConfigExportAbsentReturnsNil(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadFS(context.Background(), b, "demo.egop.wasm", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	if p.Config() != nil {
		t.Fatalf("Config() should be nil when egop_get_config absent, got %s", p.Config())
	}
}

// buildCodelessZip 生成无 plugin.wasm 的 zip 包(manifest.json + assets/)。
func buildCodelessZip(t *testing.T, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(manifest))
	w, _ = zw.Create("assets/pane.js")
	_, _ = w.Write([]byte("export default function(s){}"))
	_ = zw.Close()
	return buf.Bytes()
}

// TestCodelessZipPlugin 验证无 wasm 的 zip(纯清单/资产)可加载:
// Meta/Assets 可用、无函数可调用(报错而非 panic)、配置下发干净报错。
// 品牌后缀(.eha.zip)经 Options.ExtraSuffixes 装配注入——库内不内置业务词,
// 未注入时同一文件按"不支持的后缀"拒载。
func TestCodelessZipPlugin(t *testing.T) {
	data := buildCodelessZip(t, `{"id":"ui.only","name":"U","version":"1","extensions":{"eha.ui":{"entry":"pane.js"}}}`)

	// 未注入后缀:拒载(内容无关库不认识品牌后缀)。
	if _, err := LoadFS(context.Background(), data, "ui.eha.zip", Options{}); err == nil {
		t.Fatal("brand suffix must be rejected without ExtraSuffixes injection")
	}

	p, err := LoadFS(context.Background(), data, "ui.eha.zip", Options{ExtraSuffixes: []string{".eha.zip"}})
	if err != nil {
		t.Fatalf("codeless LoadFS: %v", err)
	}
	defer p.Close(context.Background())
	if p.Meta().ID != "ui.only" {
		t.Fatalf("meta id = %s", p.Meta().ID)
	}
	if _, ok := p.Meta().Extensions["eha.ui"]; !ok {
		t.Fatalf("extensions eha.ui missing")
	}
	if got := string(p.Assets()["pane.js"]); got != "export default function(s){}" {
		t.Fatalf("assets pane.js = %q", got)
	}
	// 无代码:CallFunc/ApplyConfig 应干净报错而非 panic。
	if _, err := p.CallFunc(context.Background(), "any", nil); err == nil {
		t.Fatalf("codeless CallFunc should error")
	}
	if err := p.ApplyConfig(nil); err == nil {
		t.Fatal("codeless ApplyConfig should error")
	}
}

// TestCodelessZipRejectsUndeliverable 无 wasm 但声明需要 guest 代码兑现的面
// (函数/工具/配置/hook 点)→ fail-closed 拒载。
func TestCodelessZipRejectsUndeliverable(t *testing.T) {
	cases := map[string]string{
		"functions": `{"id":"ui.bad","name":"B","version":"1","provides":{"functions":[{"name":"f"}]}}`,
		"tools":     `{"id":"ui.bad","name":"B","version":"1","tools":[{"name":"t"}]}`,
		"config":    `{"id":"ui.bad","name":"B","version":"1","provides":{"config":[{"key":"k"}]}}`,
		"hooks":     `{"id":"ui.bad","name":"B","version":"1","provides":{"hooks":[{"id":"h","kind":"observe"}]}}`,
	}
	for name, mf := range cases {
		data := buildCodelessZip(t, mf)
		if _, err := LoadFS(context.Background(), data, "bad.egop.zip", Options{}); err == nil {
			t.Fatalf("codeless zip declaring %s should be rejected", name)
		}
	}
}

// TestZipLimits zip 解压上限(防 zip bomb):单条目超限/聚合超限均拒载;
// 谎报头部尺寸(声明小、实际大)也被硬限读取兜住。
func TestZipLimits(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	// demo.wasm 远大于 64B:把单条目上限压到 64B 即触发拒载。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"id":"wasm.demo","name":"D","version":"1"}`))
	w, _ = zw.Create("plugin.wasm")
	_, _ = w.Write(mod)
	_ = zw.Close()
	if _, err := LoadFS(context.Background(), buf.Bytes(), "demo.egop.zip", Options{MaxEntryBytes: 64}); err == nil {
		t.Fatal("entry over MaxEntryBytes should be rejected")
	}
	// 聚合上限:整包 100B 上限,manifest+wasm 超限。
	if _, err := LoadFS(context.Background(), buf.Bytes(), "demo.egop.zip", Options{MaxTotalBytes: 100}); err == nil {
		t.Fatal("aggregate over MaxTotalBytes should be rejected")
	}
	// 默认上限内正常装载(回归:limits 不误伤正常包)。
	p, err := LoadFS(context.Background(), buf.Bytes(), "demo.egop.zip", Options{})
	if err != nil {
		t.Fatalf("normal zip rejected by limits: %v", err)
	}
	_ = p.Close(context.Background())
}

// TestZipUnsafeAssetName 资产名穿越(zip-slip 源头防护):".." / 绝对路径拒载。
func TestZipUnsafeAssetName(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(`{"id":"ui.slip","name":"S","version":"1"}`))
	w, _ = zw.Create("assets/../../evil.js")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	if _, err := LoadFS(context.Background(), buf.Bytes(), "slip.egop.zip", Options{}); err == nil {
		t.Fatal("traversing asset name should be rejected")
	}
}
