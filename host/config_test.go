// 配置读回/声明增强:ConfigProvider 权威读回(EffectiveConfig 优先)、SetConfigField
// 合并、ConfigFieldSpec.Default 声明。这些是"web 配置界面"能真正用起来的最小补齐。
package host

import (
	"encoding/json"
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
