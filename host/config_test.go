// 配置读回/声明增强:ConfigProvider 权威读回(EffectiveConfig 优先)、SetConfigField
// 合并、ConfigFieldSpec.Default 声明。这些是"web 配置界面"能真正用起来的最小补齐。
package host

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

type cpPlugin struct {
	meta contract.Meta
	cur  json.RawMessage
}

func (p *cpPlugin) Meta() contract.Meta               { return p.meta }
func (p *cpPlugin) ApplyConfig(json.RawMessage) error { return nil }
func (p *cpPlugin) Config() json.RawMessage           { return p.cur }

func TestEffectiveConfigPrefersConfigProvider(t *testing.T) {
	const authoritative = `{"api_url":"https://real","max_results":5}`
	h := New[any](Options[any]{})
	_ = h.Register(&cpPlugin{meta: contract.Meta{ID: "cp", Name: "CP", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "api_url"}, {Key: "max_results"}}}},
		cur: json.RawMessage(authoritative)})

	// 未 SetConfig 也能读到插件自述的生效配置(含默认/归一化的真值)。
	if got, ok := h.EffectiveConfig("cp"); !ok || string(got) != authoritative {
		t.Fatalf("EffectiveConfig = %s ok=%v", got, ok)
	}
	// GetConfig 单字段读自权威配置。
	if v, ok := h.GetConfig("cp", "api_url"); !ok || string(v) != `"https://real"` {
		t.Fatalf("GetConfig(api_url) = %s ok=%v", v, ok)
	}
}

func TestEffectiveConfigFallsBackToApplied(t *testing.T) {
	// 未实现 ConfigProvider 的插件 → EffectiveConfig 回退到 applied。
	h := New[any](Options[any]{})
	_ = h.Register(&demoPlugin{meta: contract.Meta{ID: "m", Name: "M", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "a"}}}}})
	_ = h.SetConfig("m", json.RawMessage(`{"a":1}`))
	if got, ok := h.EffectiveConfig("m"); !ok || string(got) != `{"a":1}` {
		t.Fatalf("EffectiveConfig = %s ok=%v", got, ok)
	}
}

func TestSetConfigFieldMerges(t *testing.T) {
	h := New[any](Options[any]{})
	_ = h.Register(&demoPlugin{meta: contract.Meta{ID: "m", Name: "M", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{{Key: "a"}, {Key: "b"}}}}})
	_ = h.SetConfig("m", json.RawMessage(`{"a":1}`))
	_ = h.SetConfigField("m", "b", json.RawMessage(`2`))

	got, _ := h.AppliedConfig("m")
	var m map[string]json.RawMessage
	_ = json.Unmarshal(got, &m)
	if len(m) != 2 || string(m["a"]) != `1` || string(m["b"]) != `2` {
		t.Fatalf("applied = %s (want merge, not replace)", got)
	}
}

// TestSetConfigFieldConcurrentNoLostUpdate 并发单字段写不丢更新:配置写链路经
// cfgMu 串行(读旧 applied→合并→下发→记录全程原子),N 个 goroutine 各写各的
// 字段,最终全部字段都在——修复前"读合写"在锁外,并发会互相覆盖丢字段。
func TestSetConfigFieldConcurrentNoLostUpdate(t *testing.T) {
	const n = 32
	fields := make([]contract.ConfigFieldSpec, 0, n)
	for i := 0; i < n; i++ {
		fields = append(fields, contract.ConfigFieldSpec{Key: "k" + strconv.Itoa(i)})
	}
	h := New[any](Options[any]{})
	if err := h.Register(&demoPlugin{meta: contract.Meta{ID: "c", Name: "C", Version: "1",
		Provides: contract.Provides{Config: fields}}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := h.SetConfigField("c", "k"+strconv.Itoa(i), json.RawMessage(strconv.Itoa(i))); err != nil {
				t.Errorf("SetConfigField(k%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, ok := h.AppliedConfig("c")
	if !ok {
		t.Fatal("no applied config after concurrent writes")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("applied = %s (%v)", got, err)
	}
	if len(m) != n {
		t.Fatalf("applied has %d fields, want %d (lost update): %s", len(m), n, got)
	}
	for i := 0; i < n; i++ {
		if string(m["k"+strconv.Itoa(i)]) != strconv.Itoa(i) {
			t.Fatalf("field k%d = %s, want %d", i, m["k"+strconv.Itoa(i)], i)
		}
	}
}
