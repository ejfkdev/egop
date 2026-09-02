// 无代码插件(资源包)示例:.egop.zip 缺 plugin.wasm = 纯清单/资产包——manifest
// 声明元数据与 Extensions(自由扩展键值),assets/ 携带静态资源,宿主经
// Meta()/Assets() 消费,全程不执行任何 guest 代码。品牌/项目自有包后缀经
// wasm.Options.ExtraSuffixes 装配注入(内容无关库不内置业务词)。
// 运行:go run .
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"testing/fstest"

	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/loader/wasm"
)

// buildPack 在内存里构造一个无代码资源包(manifest.json + assets/,无 plugin.wasm)。
func buildPack(manifest string, assets map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	_, _ = w.Write([]byte(manifest))
	for name, body := range assets {
		w, _ := zw.Create("assets/" + name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func main() {
	ctx := context.Background()

	// 资源包清单:纯元数据 + Extensions 自定义声明(egop 不解释,消费方按 key 读)。
	pack := buildPack(
		`{"id":"demo.pack","name":"Resource Pack","version":"1.0.0",`+
			`"extensions":{"demo.entry":{"file":"greeting.txt","kind":"text"}}}`,
		map[string]string{
			"greeting.txt":   "hello from a codeless bundle",
			"theme/dark.css": "body{background:#111}",
		})

	// 注入的只读 FS(内嵌/浏览器同款缝):品牌后缀 ".pack.zip" 经 ExtraSuffixes 声明。
	fsys := fstest.MapFS{
		"plugins/demo.pack.zip": {Data: pack},
		"plugins/notes.txt":     {Data: []byte("ignored: not a plugin suffix")},
	}
	extra := []string{".pack.zip"}

	// 未注入后缀:同一文件不被识别(库内只认 *.egop.wasm / *.egop.zip)。
	if plugs, _ := wasm.ScanFS(ctx, fsys, wasm.Options{}); len(plugs) != 0 {
		log.Fatalf("un-injected suffix should be ignored, got %d plugins", len(plugs))
	}
	log.Print("scan without ExtraSuffixes => ignored (content-agnostic core)")

	// 注入后:发现并装载。
	plugs, errs := wasm.ScanFS(ctx, fsys, wasm.Options{ExtraSuffixes: extra})
	if len(errs) != 0 {
		log.Fatalf("ScanFS errs: %v", errs)
	}
	if len(plugs) != 1 {
		log.Fatalf("want 1 plugin, got %d", len(plugs))
	}
	p := plugs[0]
	defer p.Close(ctx)
	log.Printf("loaded codeless bundle => %s (v%s)", p.Meta().ID, p.Meta().Version)

	// 注册进宿主:资源包与代码插件同一生命周期面(目录/事件/卸载)。
	h := host.New[any](host.Options[any]{Logf: log.Printf})
	if err := h.Register(p); err != nil {
		log.Fatal(err)
	}

	// Extensions:消费方按自己的 key 读取自定义声明(egop 原样透传)。
	var entry struct {
		File string `json:"file"`
		Kind string `json:"kind"`
	}
	if raw, ok := p.Meta().Extensions["demo.entry"]; ok {
		_ = json.Unmarshal(raw, &entry)
	}
	log.Printf("extensions demo.entry => %+v", entry)

	// Assets:按声明取出静态资源(宿主据此下发/服务,如内嵌页面或主题)。
	if body, ok := p.Assets()[entry.File]; ok {
		log.Printf("asset %s => %s", entry.File, body)
	}

	// 无代码面 fail-closed:没有 guest 代码,函数调用/配置下发都是干净错误而非 panic。
	if _, err := h.Call(ctx, "demo.pack", "anything", nil); err != nil {
		log.Printf("call on codeless => refused: %v", err)
	}
	if err := h.SetConfig("demo.pack", json.RawMessage(`{"k":1}`)); err != nil {
		log.Printf("config on codeless => refused: %v", err)
	}

	_ = h.Close(ctx)
}
