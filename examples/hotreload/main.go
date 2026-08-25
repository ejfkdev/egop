// 热更示例:mount.Sources{Watch:true} 轮询一个 wasm 插件目录,把文件的增/删映射为
// 注册/卸载(改 → 热替换,内容 hash 两段确认抗半截写)。运行:go run .
package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/ejfkdev/egop/host"
	"github.com/ejfkdev/egop/mount"
)

//go:embed demo.egop.wasm
var demoWasm []byte

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	dir, err := os.MkdirTemp("", "egop-hotreload-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rt, warns, err := mount.Mount(ctx, h, mount.Sources{
		Dirs:     []string{dir},
		Watch:    true,
		Interval: 300 * time.Millisecond,
		Logf:     log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()
	for _, w := range warns {
		log.Printf("warn: %v", w)
	}

	// 事件流(register / replace / remove / failed)。
	go func() {
		for e := range rt.Events() {
			log.Printf("event: %s %s (%s)", e.Action, e.PluginID, e.Path)
		}
	}()

	plug := filepath.Join(dir, "demo.egop.wasm")
	logStep("写入插件 → 期望 register")
	if err := os.WriteFile(plug, demoWasm, 0o644); err != nil {
		log.Fatal(err)
	}
	waitPlugin(h, "wasm.demo", true)

	logStep("删除插件 → 期望 remove")
	if err := os.Remove(plug); err != nil {
		log.Fatal(err)
	}
	waitPlugin(h, "wasm.demo", false)

	logStep("重新写入 → 再次 register(可重复装载)")
	if err := os.WriteFile(plug, demoWasm, 0o644); err != nil {
		log.Fatal(err)
	}
	waitPlugin(h, "wasm.demo", true)
}

func logStep(s string) { log.Printf("== %s ==", s) }

func waitPlugin(h *host.Host[any], id string, want bool) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.HasPlugin(id) == want {
			log.Printf("plugin %s: registered=%v", id, h.HasPlugin(id))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("timeout waiting for plugin %s registered=%v", id, want)
}
