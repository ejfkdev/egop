// 跨插件协作示例:一个插件经 plugin.meta 读目录/元数据,经 config.read/config.write
// 读写另一个插件的声明配置字段(受 Readable/Writable 字段级标志 + 能力词双重门控)。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// 提供方:声明配置字段(api_key 只写、mode 只读),并持有元数据。
type provider struct{ meta contract.Meta }

func (p *provider) Meta() contract.Meta               { return p.meta }
func (p *provider) ApplyConfig(json.RawMessage) error { return nil }

// 消费方:声明 plugin.meta + config.read + config.write,借 Surface 读目录/元数据、
// 读写其它插件的配置字段。
type consumer struct {
	meta    contract.Meta
	surface contract.Surface
}

func (c *consumer) Meta() contract.Meta           { return c.meta }
func (c *consumer) SetSurface(s contract.Surface) { c.surface = s }

func (c *consumer) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	switch fname {
	case "report":
		out := map[string]any{}
		var ids []string
		for _, m := range c.surface.Plugins() {
			ids = append(ids, m.ID)
		}
		out["plugins"] = ids
		if m, ok := c.surface.GetPlugin("app.settings"); ok {
			out["meta"] = map[string]any{"id": m.ID, "name": m.Name, "version": m.Version}
		}
		if v, ok := c.surface.GetConfig("app.settings", "mode"); ok {
			out["mode"] = string(v)
		}
		_, visible := c.surface.GetConfig("app.settings", "api_key")
		out["api_key_visible"] = visible // 只写字段读不到 → false
		b, _ := json.Marshal(out)
		return b, nil
	}
	return nil, fmt.Errorf("unknown fn %q", fname)
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf})

	prov := &provider{meta: contract.Meta{ID: "app.settings", Name: "Settings", Version: "1",
		Provides: contract.Provides{Config: []contract.ConfigFieldSpec{
			{Key: "api_key", Writable: true, Readable: false}, // 别的插件能写、读不回
			{Key: "mode", Writable: false, Readable: true},    // 只读
		}}}}
	cons := &consumer{meta: contract.Meta{ID: "app.worker", Name: "Worker", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapPluginMeta, contract.CapConfigRead, contract.CapConfigWrite},
			Functions:    []contract.FuncSpec{{Name: "report"}},
		}}}
	blind := &provider{meta: contract.Meta{ID: "app.blind", Name: "Blind", Version: "1"}}

	rep := h.RegisterMany([]contract.Plugin{prov, cons, blind})
	log.Printf("registered=%v failed=%d", rep.Registered, len(rep.Failed))

	// egop 下发初始配置(宿主层,始终可读写)。
	if err := h.SetConfig("app.settings", json.RawMessage(`{"api_key":"sk-live","mode":"prod"}`)); err != nil {
		log.Fatal(err)
	}

	// 消费方用自己的 Surface 读目录/元数据/可读配置。
	report, err := h.Call(ctx, "app.worker", "report", json.RawMessage(`{}`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("worker.report() = %s\n", report)

	// 消费方写「只写」字段(需 config.write + 字段 writable)。
	ws, _ := h.SurfaceFor("app.worker")
	if err := ws.SetConfig("app.settings", "api_key", json.RawMessage(`"sk-rotated"`)); err != nil {
		log.Fatal(err)
	}
	v, _ := h.GetConfig("app.settings", "api_key")
	log.Printf("consumer wrote app.settings.api_key = %s (egop reads it back)", v)

	// 盲插件(未声明 plugin.meta)什么也看不到。
	bs, _ := h.SurfaceFor("app.blind")
	_, seesSettings := bs.GetPlugin("app.settings")
	log.Printf("blind sees %d plugins (want 0); sees app.settings=%v (want false)",
		len(bs.Plugins()), seesSettings)

	if err := h.Close(ctx); err != nil {
		log.Fatal(err)
	}
}
