package wasm

import (
	"encoding/json"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// mkPoolPlugin 造一个只带清单扩展的插件(不实例化 guest)。
func mkPoolPlugin(ext map[string]json.RawMessage) *Plugin {
	p := newPlugin("t")
	p.manifest = contract.Manifest{Meta: contract.Meta{Extensions: ext}}
	return p
}

// TestPoolSizeAdaptive 锁「池上限按主机资源自适应 + 声明被上限夹 + auto 铺满」:
// 固定 8 的旧护栏会让并发 agent 静默排队(2026-11-09 bench9),所以这里断言的
// 是不变量而不是某个魔数。
func TestPoolSizeAdaptive(t *testing.T) {
	t.Setenv("EGOP_POOL_CEIL", "12")

	cases := []struct {
		name string
		ext  map[string]json.RawMessage
		want int
	}{
		{"缺省=单实例", nil, 1},
		{"声明小于上限=照声明", map[string]json.RawMessage{"egop.pool": json.RawMessage("4")}, 4},
		{"声明大于上限=夹到上限", map[string]json.RawMessage{"egop.pool": json.RawMessage("64")}, 12},
		{"auto=铺满上限", map[string]json.RawMessage{"egop.pool": json.RawMessage(`"auto"`)}, 12},
		{"auto 大小写/空格容错", map[string]json.RawMessage{"egop.pool": json.RawMessage(`" Auto "`)}, 12},
		{"非法字符串=退回单实例", map[string]json.RawMessage{"egop.pool": json.RawMessage(`"lots"`)}, 1},
		{"0/负数=非法", map[string]json.RawMessage{"egop.pool": json.RawMessage("0")}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mkPoolPlugin(c.ext).poolSize(); got != c.want {
				t.Fatalf("poolSize=%d want %d", got, c.want)
			}
		})
	}
}

// TestPoolCeilingFromHost 未设 env 时,上限必须是「主机内存/单实例最坏内存」推出
// 的正数且 ≥8(内存未知时退回 CPU 规模);绝不能退回旧的固定 8 语义。
func TestPoolCeilingFromHost(t *testing.T) {
	t.Setenv("EGOP_POOL_CEIL", "")
	p := mkPoolPlugin(map[string]json.RawMessage{"egop.memory_pages": json.RawMessage("8192")})
	if got := p.instMaxBytes(); got != 8192*64*1024 {
		t.Fatalf("instMaxBytes=%d want %d", got, 8192*64*1024)
	}
	n := p.poolCeiling()
	if n < 8 || n > absPoolCeil {
		t.Fatalf("poolCeiling=%d 越界(应 [8,%d])", n, absPoolCeil)
	}
	// 本机若是 linux/darwin,一定探到内存,池上限应明显大于旧护栏 8。
	if hostMemoryBytes() > 0 && n <= 8 {
		t.Fatalf("poolCeiling=%d 未随主机内存放大(hostMemoryBytes=%d)", n, hostMemoryBytes())
	}
}

// TestPoolCeilEnvClamp EGOP_POOL_CEIL 的非法值与上界。
func TestPoolCeilEnvClamp(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int
	}{
		{"", 0}, {"0", 0}, {"-3", 0}, {"abc", 0}, {" 16 ", 16}, {"9999", absPoolCeil},
	} {
		t.Setenv("EGOP_POOL_CEIL", c.v)
		if got := envPoolCeil(); got != c.want {
			t.Fatalf("EGOP_POOL_CEIL=%q → %d want %d", c.v, got, c.want)
		}
	}
}

// TestPoolMaxCap 清单 egop.pool_max 的帽子:auto/声明值都要被压到帽内。
func TestPoolMaxCap(t *testing.T) {
	t.Setenv("EGOP_POOL_CEIL", "40")
	mk := func(ext map[string]json.RawMessage) *Plugin {
		p := newPlugin("t")
		p.manifest = contract.Manifest{Meta: contract.Meta{Extensions: ext}}
		return p
	}
	auto := mk(map[string]json.RawMessage{
		"egop.pool":     json.RawMessage(`"auto"`),
		"egop.pool_max": json.RawMessage("6"),
	})
	if got := auto.poolSize(); got != 6 {
		t.Fatalf("auto+cap = %d want 6", got)
	}
	big := mk(map[string]json.RawMessage{
		"egop.pool":     json.RawMessage("30"),
		"egop.pool_max": json.RawMessage("10"),
	})
	if got := big.poolSize(); got != 10 {
		t.Fatalf("声明 30 + cap 10 = %d want 10", got)
	}
	// 帽大于声明:不放大
	small := mk(map[string]json.RawMessage{
		"egop.pool":     json.RawMessage("3"),
		"egop.pool_max": json.RawMessage("10"),
	})
	if got := small.poolSize(); got != 3 {
		t.Fatalf("声明 3 + cap 10 = %d want 3", got)
	}
}
