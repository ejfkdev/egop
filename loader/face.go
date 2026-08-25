// loader 包聚合加载器家族:本文件定义装配组件的宿主最小面(HostFace)——
// core host.Host[C] 天然满足;非 core 宿主(如外部自有宿主)包一层桥即接入
// 全部加载器(wasm 目录/热更/远程通道双方向),不必重写任何加载代码。
package loader

import (
	"context"
	"encoding/json"

	"github.com/ejfkdev/egop/contract"
)

// HostFace 是加载器所需的宿主最小面。装配组件(autoload/mount)只依赖它,
// 与宿主泛化参数 C 解耦。
type HostFace interface {
	// Register 登记插件(重复 id/依赖不足/槽位契约不满足 = 错误)。
	Register(p contract.Plugin) error
	// Replace 同 id 热替换(旧版在册时替换,否则按新注册处理由宿主裁决)。
	Replace(p contract.Plugin) error
	// Remove 级联卸载(cascade=false 且被依赖时 fail-closed)。
	Remove(pluginID string, cascade bool) ([]string, error)
	// HasPlugin / Plugins 元数据查询面。
	HasPlugin(id string) bool
	Plugins() []contract.Meta
	// SetConfig 下发配置(宿主校验 + Configurable);AppliedConfig 已生效配置。
	SetConfig(pluginID string, cfg json.RawMessage) error
	AppliedConfig(pluginID string) (json.RawMessage, bool)
	// SurfaceFor 能力门控 Surface 视图(远程插件 HostCall 回程路由入口)。
	SurfaceFor(pluginID string) (contract.Surface, bool)
	// Call 动态调用函数(热更自检/调用方消费)。
	Call(ctx context.Context, pluginID, fname string, input json.RawMessage) (json.RawMessage, error)
}
