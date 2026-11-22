package wasm

// egop.memory_pages 清单扩展:重插件可申请更高线性内存上限(默认 256MiB,
// 上限 1GiB);全局 Options 作为基线,清单更大时取清单。

import (
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func pluginWithMemPages(v string) *Plugin {
	ext := map[string]json.RawMessage{}
	if v != "" {
		ext["egop.memory_pages"] = json.RawMessage(v)
	}
	return &Plugin{manifest: contract.Manifest{Meta: contract.Meta{Extensions: ext}}}
}

func TestMemoryPagesDefaults(t *testing.T) {
	if got := pluginWithMemPages("").memoryPages(Options{}); got != DefaultMaxPages {
		t.Fatalf("默认 = %d, want %d", got, DefaultMaxPages)
	}
}

func TestMemoryPagesManifestRaises(t *testing.T) {
	if got := pluginWithMemPages("8192").memoryPages(Options{}); got != 8192 {
		t.Fatalf("清单 8192 未生效: %d", got)
	}
	// 全局基线更低时,清单可放大(重插件声明额度)。
	if got := pluginWithMemPages("8192").memoryPages(Options{MaxMemoryPages: 2048}); got != 8192 {
		t.Fatalf("清单应可放大到 8192: %d", got)
	}
}

func TestMemoryPagesClampAndInvalid(t *testing.T) {
	if got := pluginWithMemPages("999999").memoryPages(Options{}); got != MaxPagesCeiling {
		t.Fatalf("应钳到天花板 %d, got %d", MaxPagesCeiling, got)
	}
	for _, bad := range []string{`"8"`, `0`, `-1`, `not-json`} {
		if got := pluginWithMemPages(bad).memoryPages(Options{}); got != DefaultMaxPages {
			t.Fatalf("非法值 %s 应回落默认, got %d", bad, got)
		}
	}
}
