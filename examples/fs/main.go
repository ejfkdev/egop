// 跨平台只读注入缝示例:直接把插件字节交给 LoadFS,或把任意 io/fs.FS(如内存
// 文件系统/自定义实现)喂给 ScanFS——全程不碰 OS 文件系统,浏览器/内嵌同样可用。
// 运行:go run .
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing/fstest"

	"github.com/ejfkdev/egop/loader/wasm"
)

func main() {
	ctx := context.Background()

	// 插件字节(浏览器里可来自 fetch/网络/代码,这里读自本目录夹具)。
	raw, err := os.ReadFile(filepath.Join("testdata", "demo.wasm"))
	if err != nil {
		log.Fatal(err)
	}

	// 1) 直接给字节:不依赖任何目录结构。
	p, err := wasm.LoadFS(ctx, raw, "demo.egop.wasm", wasm.Options{})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("LoadFS(bytes) => %s", p.Meta().ID)
	_ = p.Close(ctx)

	// 2) 目录发现走注入的只读 FS——这里用内存 MapFS 演示,生产可换成浏览器/自定义 FS。
	fsys := fstest.MapFS{
		"plugins/demo.egop.wasm": &fstest.MapFile{Data: raw},
		"plugins/ignored.txt":    &fstest.MapFile{Data: []byte("junk")}, // 非插件后缀,忽略
	}
	plugs, errs := wasm.ScanFS(ctx, fsys, wasm.Options{})
	if len(errs) != 0 {
		log.Fatalf("ScanFS errs: %v", errs)
	}
	for _, p := range plugs {
		log.Printf("ScanFS(fs.FS) => %s", p.Meta().ID)
		_ = p.Close(ctx)
	}
}
