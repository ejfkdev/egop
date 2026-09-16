// Plugin 是 WASM 插件适配器:一个 base.Plugin 实现,把 base 的可选接口
// (FunctionProvider/ToolProvider/Configurable/SurfaceAware)逐一翻译为 guest ABI 调用。
// 注册进 pkg/plugin.Host 后,函数目录、能力门控、事件订阅、工具合并等
// 全部走既有宿主机机制——进程内插件与远程插件同一套生命周期。
package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/tetratelabs/wazero"
)

type Plugin struct {
	name     string // 展示名(通常为文件名)
	manifest contract.Manifest
	runtime  wazero.Runtime
	compiled wazero.CompiledModule // 池化锚:实例重建(revive)不再重编译
	insts    []*inst
	byName   map[string]*inst // wazero module 名 → inst(宿主注入函数经 m 认回)
	poolMu   sync.Mutex       // 保护 insts/byName/compiled/lastCfg(生命周期读写收口)
	// surface 经原子指针:宿主注入函数(热路径)无锁读,SetSurface 单写。
	// 注册时序保证单写;读侧 surf() 无锁。
	surfacePtr atomic.Pointer[contract.Surface]
	assets     map[string][]byte // .egop.zip 内 assets/ 静态文件(只读)
	logFn      func(level, msg string)
	closed     atomic.Bool // 显式 Close():revive 不适用(注销/关停是终态)

	// revive 三件套:guest 代码字节 + 装载选项 + 最近生效配置——意外打断(ctx 取消
	// 看门狗/trap)后按需重建实例并回放 init/config(插件内存态归零=热重启一次)。
	wasmRaw  []byte
	loadOpts Options
	lastCfg  json.RawMessage
}

func newPlugin(name string) *Plugin {
	return &Plugin{name: name, assets: map[string][]byte{}, byName: map[string]*inst{}}
}

// surf 读注入的 Surface 视图(未接线返回 nil)。原子读:宿主注入热路径无锁。
func (p *Plugin) surf() contract.Surface {
	if s := p.surfacePtr.Load(); s != nil {
		return *s
	}
	return nil
}

// compiledModule 在 poolMu 下快照已编译模块(revive/growPool 用;Close 的
// 关闭/置 nil 同锁——读写竞争收口)。
func (p *Plugin) compiledModule() wazero.CompiledModule {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	return p.compiled
}

// asset 读打包静态资源(装载后只读,无需锁)。
func (p *Plugin) asset(name string) ([]byte, bool) {
	b, ok := p.assets[name]
	return b, ok
}

// Meta 实现 base.Plugin。
func (p *Plugin) Meta() contract.Meta { return p.manifest.Meta }

// Assets 返回 .egop.zip 内 assets/ 静态资源表的只读副本(资源名→字节)。
// 宿主据此下发插件自带的静态资产;裸 .egop.wasm 形态返回空表。
// 返回值是副本:调用方改 map 不影响插件内部;字节切片本身视为只读。
func (p *Plugin) Assets() map[string][]byte {
	out := make(map[string][]byte, len(p.assets))
	for k, v := range p.assets {
		out[k] = v
	}
	return out
}

// Config 实现 contract.ConfigProvider:调 guest 的 egop_get_config 导出读回当前生效
// 配置(权威读回,取任一空闲 inst);未导出/失败返回 nil → 宿主 EffectiveConfig 回退 applied 缓存。
func (p *Plugin) Config() json.RawMessage {
	ctx, cancel := context.WithTimeout(context.Background(), applyConfigTimeout)
	defer cancel()
	i, err := p.acquire(ctx)
	if err != nil {
		return nil
	}
	defer p.release(i)
	if i.broken.Load() || i.mod == nil {
		return nil
	}
	fn := i.mod.ExportedFunction(ExportGetConfig)
	if fn == nil {
		return nil
	}
	results, err := fn.Call(ctx)
	if err != nil || len(results) != 1 {
		return nil
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(i.mod.Memory(), ptr, ln)
	if err != nil {
		return nil
	}
	return json.RawMessage(s)
}

// CallFunc 实现 base.FunctionProvider。
func (p *Plugin) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	i, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer p.release(i)
	// 意外打断自愈:上一调用被 ctx 取消/trap 打死 → 本次先重建实例(显式 Close 不复活)。
	if i.broken.Load() {
		if err := p.reviveLocked(ctx, i); err != nil {
			return nil, err
		}
	}
	// 持有标记:本调用栈持有本插件实例——嵌套 acquire(经宿主注入回调回同一插件)
	// 据此识别同栈重入,池耗尽时立即 busy 回落而非等待自死锁。
	ctx = withHeld(ctx, p)
	args := []string{fname, string(input)}
	// egop_call ABI 两版:4 参(fname,in)= 旧版 / 6 参(+origin)= 新版(第 3 参=
	// 调用方来源 Origin 裸 JSON,SDK guest 读它并 WithOrigin 还原,使插件函数能经
	// OriginFrom 知道"谁调了我";ctx 本身无法跨 wasm ABI)。精确匹配 6 才传第三参;
	// 其它非法元数在装载期 validate 已按 ABI 不合规拒载。
	if fn := i.mod.ExportedFunction(ExportCall); fn != nil && len(fn.Definition().ParamTypes()) == 6 {
		originJSON, _ := json.Marshal(contract.OriginFrom(ctx))
		args = append(args, string(originJSON))
	}
	return i.callExport(p, ctx, ExportCall, args...)
}

// toolSpecsTimeout 活体工具面查询的兜底超时(同 ApplyConfig 形:接口无 ctx,
// 在此设上限防 guest 挂起拖死收集方)。
const toolSpecsTimeout = 10 * time.Second

// ToolSpecs 实现 base.ToolProvider:**活体查询优先**(guest 导出
// egop_tool_specs 时——动态工具插件如 MCP 的运行期发现真源),无导出/失败/
// 实例已 broken 回落线上清单静态表(向后兼容旧 guest)。
func (p *Plugin) ToolSpecs() []contract.FuncSpec {
	ctx, cancel := context.WithTimeout(context.Background(), toolSpecsTimeout)
	defer cancel()
	if i, err := p.acquire(ctx); err == nil {
		ctx = withHeld(ctx, p) // 同 CallFunc:嵌套重入标记
		live := false
		var out json.RawMessage
		if i.mod != nil && !i.broken.Load() { // 注册期 mod 未挂:回落清单(装配序守卫)
			if o, err := i.callExport(p, ctx, ExportToolSpecs); err == nil {
				out, live = o, true
			}
		}
		p.release(i)
		if live {
			var specs []contract.FuncSpec
			if json.Unmarshal(out, &specs) == nil {
				return specs
			}
		}
	}
	return p.manifest.Tools
}

// ToolRaw 按名返回**无类型工具执行**闭包:tctx 即线上 JSON(ABI 同形;
// 调用方负责把工具上下文序列化为该插件上下文的最小形状)。
// 结果以字符串形态返回供消费方直用。
func (p *Plugin) ToolRaw(name string) (func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error), bool) {
	known := false
	for _, s := range p.manifest.Tools {
		if s.Name == name {
			known = true
			break
		}
	}
	if !known {
		// 动态工具面:清单未录 ≠ 不存在——活体规格再查一遍(MCP 运行期发现)。
		for _, s := range p.ToolSpecs() {
			if s.Name == name {
				known = true
				break
			}
		}
	}
	if known {
		return func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error) {
			i, err := p.acquire(ctx)
			if err != nil {
				return "", err
			}
			defer p.release(i)
			if i.broken.Load() { // 意外打断自愈(同 CallFunc)
				if err := p.reviveLocked(ctx, i); err != nil {
					return "", err
				}
			}
			ctx = withHeld(ctx, p) // 同 CallFunc:嵌套重入标记
			out, err := i.callExport(p, ctx, ExportTool, name, string(args), string(tctxJSON))
			if err != nil {
				return "", err
			}
			return string(out), nil
		}, true
	}
	return nil, false
}

// applyConfigTimeout 是 guest 应用配置的兜底超时(Configurable 接口本身无 ctx,
// 只能在此设上限,避免 guest 挂起时 host.SetConfig 无限阻塞)。
const applyConfigTimeout = 10 * time.Second

// initTimeout / shutdownTimeout 是生命周期钩子(egop_init/egop_shutdown)的兜底超时:
// wasm guest 是纯计算(无 fs/网络注入),合理耗时远小于此;超时即经看门狗打断并
// fail-closed(init 失败置 broken、shutdown 失败记错后仍继续关 module/runtime)。
const (
	initTimeout     = 30 * time.Second
	shutdownTimeout = 10 * time.Second
)

// ApplyConfig 实现 base.Configurable:全池下发(guest 未导出 egop_apply_config 时
// 拒绝下发;无代码包无 guest 可下发,返回干净错误而非 nil 解引用)。
func (p *Plugin) ApplyConfig(cfg json.RawMessage) error {
	p.poolMu.Lock()
	n := len(p.insts)
	p.poolMu.Unlock()
	if n == 0 {
		return fmt.Errorf("wasm plugin %s: codeless bundle is not configurable", p.name)
	}
	err := p.eachInst(func(i *inst) error {
		if i.mod.ExportedFunction(ExportApplyConfig) == nil {
			return fmt.Errorf("wasm plugin %s: export %q missing (not configurable)", p.name, ExportApplyConfig)
		}
		ctx, cancel := context.WithTimeout(context.Background(), applyConfigTimeout)
		defer cancel()
		if _, err := i.callExport(p, ctx, ExportApplyConfig, string(cfg)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.poolMu.Lock()
	p.lastCfg = append(json.RawMessage(nil), cfg...) // revive 回放锚
	p.poolMu.Unlock()
	return nil
}

// SetSurface 实现 base.SurfaceAware:注册时注入能力门控 Surface 视图,并对全池
// 执行 egop_init(若有导出)。初始化失败的实例置 broken(后续调用 fail-closed 并
// 在 acquire 时 revive),注册本身不失败。
func (p *Plugin) SetSurface(s contract.Surface) {
	p.surfacePtr.Store(&s)
	_ = p.eachInst(func(i *inst) error {
		if i.mod != nil && i.mod.ExportedFunction(ExportInit) != nil {
			// SetSurface 无 ctx:用固定兜底超时接看门狗,guest 挂起致 egop_init 死循环时打断。
			ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
			defer cancel()
			if _, err := i.callExport(p, ctx, ExportInit); err != nil {
				i.broken.Store(true)
			}
		}
		return nil
	})
}

// Close 关闭全池:撤销订阅、尽力执行 egop_shutdown、关模块与运行时。
// 显式关闭是终态:之后的调用不再 revive(与意外打断的可恢复语义区分)。
func (p *Plugin) Close(ctx context.Context) error {
	p.closed.Store(true)
	var errs []error
	p.poolMu.Lock()
	insts := append([]*inst(nil), p.insts...)
	p.insts = nil
	p.byName = map[string]*inst{}
	p.poolMu.Unlock()
	for _, i := range insts {
		i.mu.Lock()
		if err := i.unsubs.Close(); err != nil {
			errs = append(errs, err)
		}
		if i.mod != nil {
			if i.mod.ExportedFunction(ExportShutdown) != nil {
				// 优雅关停设兜底超时(调用方 ctx 常为 Background);超时经看门狗打断后仍续关 module/runtime。
				sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
				if _, err := i.callExport(p, sctx, ExportShutdown); err != nil {
					errs = append(errs, err)
				}
				cancel()
			}
			if err := i.mod.Close(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		i.netCloseAll()
		i.broken.Store(true)
		i.mu.Unlock()
	}
	p.poolMu.Lock()
	compiled := p.compiled
	p.compiled = nil
	p.poolMu.Unlock()
	if compiled != nil {
		if err := compiled.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if p.runtime != nil {
		if err := p.runtime.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// callMeta 无信封特殊通道:仅 egop_meta 用(裸 manifest JSON;装载期单实例上下文)。
func (p *Plugin) callMeta(ctx context.Context) (json.RawMessage, error) {
	p.poolMu.Lock()
	i := p.insts[0]
	p.poolMu.Unlock()
	if i == nil {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.broken.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	fn := i.mod.ExportedFunction(ExportMeta)
	if fn == nil {
		return nil, fmt.Errorf("wasm plugin %s: export %q missing", p.name, ExportMeta)
	}
	results, err := fn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, ExportMeta, err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("wasm plugin %s: %s: bad result arity", p.name, ExportMeta)
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(i.mod.Memory(), ptr, ln)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, ExportMeta, err)
	}
	return json.RawMessage(s), nil
}
