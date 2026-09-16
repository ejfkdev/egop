package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ejfkdev/egop/host"
)

// TestSysWallclock guest 读 WASI realtime 钟必须拿到真实当前时间。
// wazero RuntimeConfig 缺省 FakeWallclock(纪元 2022-01-01)会让插件内一切
// 时间逻辑(TTL/排程/退避/时间戳)静默错乱——load.go 的 WithSysWallclock
// 是本测试固化的行为,勿删。夹具 testdata/clock.wat:check 函数返回
// "fresh"(墙钟秒数 > 2026-01-01)或 "stale"。
func TestSysWallclock(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "clock.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadFS(context.Background(), b, "clock.egop.wasm", Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer p.Close(context.Background())
	h := host.New[any](host.Options[any]{})
	if err := h.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	out, err := h.Call(context.Background(), "wasm.clock", "check", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result %s: %v", out, err)
	}
	if got != "fresh" {
		t.Fatalf("guest wallclock = %q, want fresh (WithSysWallclock missing → FakeWallclock 2022)", got)
	}
}
