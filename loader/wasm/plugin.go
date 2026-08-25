// Plugin 是 WASM 插件适配器:一个 base.Plugin 实现,把 base 的可选接口
// (FunctionProvider/ToolProvider/Configurable/SurfaceAware)逐一翻译为 guest ABI 调用。
// 注册进 pkg/plugin.Host 后,函数目录、能力门控、事件订阅、工具合并等
// 全部走既有宿主机机制——进程内插件与远程插件同一套生命周期。
package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/undo"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Plugin 是 WASM 插件的宿主侧实体。guest 实例非并发安全,全部跨边界入口
// (函数/工具/配置/事件回调/关闭)经 mu 串行化;宿主注入函数在同一把锁内执行,不可重入取锁。
type Plugin struct {
	name     string // 展示名(通常为文件名)
	manifest contract.Manifest
	runtime  wazero.Runtime
	mod      api.Module
	mu       sync.Mutex
	surface  contract.Surface
	assets   map[string][]byte // .egop.zip 内 assets/ 静态文件(只读)
	logFn    func(level, msg string)
	unsubs   undo.Catcher // 事件订阅撤销统一栈(Close 释放)
	broken   atomic.Bool  // 实例已关闭/被取消:后续调用 fail-closed
}

func newPlugin(name string) *Plugin {
	return &Plugin{name: name, assets: map[string][]byte{}}
}

// asset 读打包静态资源(宿主注入函数上下文:已持锁)。
func (p *Plugin) asset(name string) ([]byte, bool) {
	b, ok := p.assets[name]
	return b, ok
}

// recordUnsub 记录订阅撤销函数(同一把锁内;统一 effect 栈)。
func (p *Plugin) recordUnsub(fn func()) { p.unsubs.Defer(fn) }

// Meta 实现 base.Plugin。
func (p *Plugin) Meta() contract.Meta { return p.manifest.Meta }

// Config 实现 contract.ConfigProvider:调 guest 的 egop_get_config 导出读回当前生效
// 配置(权威读回);未导出/失败返回 nil → 宿主 EffectiveConfig 回退 applied 缓存。
func (p *Plugin) Config() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken.Load() || p.mod == nil {
		return nil
	}
	fn := p.mod.ExportedFunction(ExportGetConfig)
	if fn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyConfigTimeout)
	defer cancel()
	results, err := fn.Call(ctx)
	if err != nil || len(results) != 1 {
		return nil
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(p.mod.Memory(), ptr, ln)
	if err != nil {
		return nil
	}
	return json.RawMessage(s)
}

// CallFunc 实现 base.FunctionProvider。
func (p *Plugin) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callExportLocked(ctx, ExportCall, fname, string(input))
}

// ToolSpecs 实现 base.ToolProvider(来自线上清单 Manifest.Tools)。
func (p *Plugin) ToolSpecs() []contract.FuncSpec { return p.manifest.Tools }

// ToolRaw 按名返回**无类型工具执行**闭包:tctx 即线上 JSON（ABI 同形;
// 调用方负责把工具上下文序列化为该插件上下文的最小形状）。
// 结果以字符串形态返回供消费方直用。
func (p *Plugin) ToolRaw(name string) (func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error), bool) {
	for _, s := range p.manifest.Tools {
		if s.Name == name {
			return func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error) {
				p.mu.Lock()
				defer p.mu.Unlock()
				out, err := p.callExportLocked(ctx, ExportTool, name, string(args), string(tctxJSON))
				if err != nil {
					return "", err
				}
				return string(out), nil
			}, true
		}
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

// ApplyConfig 实现 base.Configurable(guest 未导出 egop_apply_config 时拒绝下发)。
func (p *Plugin) ApplyConfig(cfg json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mod.ExportedFunction(ExportApplyConfig) == nil {
		return fmt.Errorf("wasm plugin %s: export %q missing (not configurable)", p.name, ExportApplyConfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), applyConfigTimeout)
	defer cancel()
	if _, err := p.callExportLocked(ctx, ExportApplyConfig, string(cfg)); err != nil {
		return err
	}
	return nil
}

// SetSurface 实现 base.SurfaceAware:注册时注入能力门控 Surface 视图,并执行 egop_init(若有导出)。
// 初始化失败 = 实例置 broken(后续调用 fail-closed),注册本身不失败。
func (p *Plugin) SetSurface(s contract.Surface) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.surface = s
	if p.mod != nil && p.mod.ExportedFunction(ExportInit) != nil {
		// SetSurface 无 ctx:用固定兜底超时接看门狗,guest 挂起致 egop_init 死循环时打断。
		ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
		defer cancel()
		if _, err := p.callExportLocked(ctx, ExportInit); err != nil {
			p.broken.Store(true)
		}
	}
}

// pushEvent 是订阅回调:把事件经 egop_on_event 推给 guest(尽力而为,未导出回调则丢弃)。
// 统一事件结构:整个 contract.Event(含 Source/Labels)JSON 作为单参传给 guest。
func (p *Plugin) pushEvent(ctx context.Context, _ string, e contract.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken.Load() || p.mod == nil {
		return
	}
	fn := p.mod.ExportedFunction(ExportOnEvent)
	if fn == nil {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	ptr, err := allocWriteGuest(ctx, p.mod, raw)
	if err != nil {
		return
	}
	_, _ = fn.Call(ctx, uint64(ptr), uint64(len(raw))) // 事件投递 best-effort:失败静默(观察面不拦不改)
}

// invokeHook 把 hook 触发转发给 guest 的 egop_on_hook 导出,解其返回信封的
// result 得到 HookResult(Block/Reason/Data;Who/At/Seq 由框架回填)。
func (p *Plugin) invokeHook(ctx context.Context, hookID string, data json.RawMessage) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken.Load() || p.mod == nil {
		return contract.HookResult{Reason: "instance closed"}
	}
	fn := p.mod.ExportedFunction(ExportOnHook)
	if fn == nil {
		return contract.HookResult{}
	}
	args := []string{hookID, string(data)}
	params := make([]uint64, 0, 4)
	for _, a := range args {
		ptr, err := allocWriteGuest(ctx, p.mod, []byte(a))
		if err != nil {
			return contract.HookResult{Reason: err.Error()}
		}
		params = append(params, uint64(ptr), uint64(len(a)))
	}
	results, err := fn.Call(ctx, params...)
	if err != nil {
		return contract.HookResult{Reason: err.Error()}
	}
	if len(results) != 1 {
		return contract.HookResult{Reason: "bad result arity"}
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(p.mod.Memory(), ptr, ln)
	if err != nil {
		return contract.HookResult{Reason: err.Error()}
	}
	var env envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return contract.HookResult{Reason: "bad result envelope"}
	}
	if !env.OK {
		return contract.HookResult{Reason: env.Error}
	}
	var hr contract.HookResult
	if len(env.Result) > 0 {
		_ = json.Unmarshal(env.Result, &hr)
	}
	return hr
}

// Close 关闭实例:撤销订阅、尽力执行 egop_shutdown、关模块与运行时。
func (p *Plugin) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	if err := p.unsubs.Close(); err != nil {
		errs = append(errs, err)
	}
	if p.mod != nil {
		if p.mod.ExportedFunction(ExportShutdown) != nil {
			// 优雅关停设兜底超时(调用方 ctx 常为 Background);超时经看门狗打断后仍续关 module/runtime。
			sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
			if _, err := p.callExportLocked(sctx, ExportShutdown); err != nil {
				errs = append(errs, err)
			}
			cancel()
		}
		if err := p.mod.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if p.runtime != nil {
		if err := p.runtime.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	p.broken.Store(true)
	return errors.Join(errs...)
}

// callExportLocked 要求已持锁(宿主注入函数/SetSurface/Close 上下文复用)。
// ctx 取消 → 看门狗经 CloseWithExitCode 打断 guest 并把实例置 broken。
func (p *Plugin) callExportLocked(ctx context.Context, fname string, args ...string) (json.RawMessage, error) {
	if p.broken.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	fn := p.mod.ExportedFunction(fname)
	if fn == nil {
		return nil, fmt.Errorf("wasm plugin %s: export %q missing", p.name, fname)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	defer close(done)
	var finished atomic.Bool
	if ctx.Done() != nil { // 可取消才装看门狗;Background 不浪费每调 goroutine
		go func() {
			select {
			case <-done:
			case <-ctx.Done():
				if finished.Load() {
					return // 调用已完成:迟到的取消不再误打断/误置 broken
				}
				_ = p.mod.CloseWithExitCode(ctx, 255)
				p.broken.Store(true)
			}
		}()
	}
	params := make([]uint64, 0, len(args)*2)
	for _, a := range args {
		ptr, err := allocWriteGuest(ctx, p.mod, []byte(a))
		if err != nil {
			return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
		}
		params = append(params, uint64(ptr), uint64(len(a)))
	}
	results, err := fn.Call(ctx, params...)
	finished.Store(true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("wasm plugin %s: %s: interrupted: %w", p.name, fname, ctx.Err())
		}
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("wasm plugin %s: %s: bad result arity", p.name, fname)
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(p.mod.Memory(), ptr, ln)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
	}
	var env envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: bad result envelope: %w", p.name, fname, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("wasm plugin %s: %s: %s", p.name, fname, env.Error)
	}
	if env.ResultB64 != "" {
		data, err := base64.StdEncoding.DecodeString(env.ResultB64)
		if err != nil {
			return nil, fmt.Errorf("wasm plugin %s: %s: bad result_b64: %w", p.name, fname, err)
		}
		return data, nil
	}
	return env.Result, nil
}

// callMeta 无信封特殊通道:仅 egop_meta 用(裸 manifest JSON)。
func (p *Plugin) callMeta(ctx context.Context) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	fn := p.mod.ExportedFunction(ExportMeta)
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
	s, err := readGuestString(p.mod.Memory(), ptr, ln)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, ExportMeta, err)
	}
	return json.RawMessage(s), nil
}
