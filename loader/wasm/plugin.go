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
	// waitMu/waiters:acquire 的 FIFO 公平队列(2026-10-15;锁序恒 waitMu→
	// poolMu)。等待者持缓冲 1 的 chan,release 以所有权交接递实例(锁不放开)。
	waitMu  sync.Mutex
	waiters []chan *inst
	// surface 经原子指针:宿主注入函数(热路径)无锁读,SetSurface 单写。
	// 注册时序保证单写;读侧 surf() 无锁。
	surfacePtr atomic.Pointer[contract.Surface]
	assets     map[string][]byte // .egop.zip 内 assets/ 静态文件(只读)
	logFn      func(level, msg string)
	closed     atomic.Bool // 显式 Close():revive 不适用(注销/关停是终态)

	// memPages 本插件生效的线性内存上限(页;memory_pages import 供 guest 自适应
	// 软堆顶)。实例化前由 memoryPages() 解析并写入。
	memPages uint32
	// revive 三件套:guest 代码字节 + 装载选项 + 最近生效配置——意外打断(ctx 取消
	// 看门狗/trap)后按需重建实例并回放 init/config(插件内存态归零=热重启一次)。
	wasmRaw  []byte
	loadOpts Options
	lastCfg  json.RawMessage

	// toolCache 活体工具面快照(ToolSpecs 非阻塞化,2026-10-31):元数据查询是
	// 装配热路径(每次 agent 启动全量枚举宿主工具),不得排在长调用(LLM 流式
	// 轮次/整轮 react)后面等实例——否则每个忙插件烧满查询超时,启动被拖到
	// 10s×N。口径:新鲜快照直接返回;否则仅在**无等待者且有空闲实例**时做
	// 活体查询,忙则回旧快照(再退清单静态表),动态发现(MCP)的新鲜度由 TTL
	// 保证,不牺牲启动延迟。
	toolMu      sync.Mutex
	toolCache   []contract.FuncSpec
	toolCacheAt time.Time
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

// toolSpecsTTL 活体工具面快照复用窗口:动态发现插件(MCP)的工具集变化以
// 秒级延迟可见,换取装配热路径(每次 agent 启动)永不排队等实例。
const toolSpecsTTL = 5 * time.Second

// ToolSpecs 实现 base.ToolProvider:**快照优先,空/过期且实例空闲时活体刷新**。
// 活体查询(guest 导出 egop_tool_specs)是动态工具插件如 MCP 的运行期发现真源,
// 但实例被长调用(LLM 流式轮次/整轮 react)占用时,排队查询会烧满超时并拖死
// 调用方(实测:每次 agent 启动被两个忙插件各拖 10s)。改为非阻塞:忙=回旧快照,
// 无快照=回线上清单静态表;绝不为元数据排队,也不插真实工作的队。
func (p *Plugin) ToolSpecs() []contract.FuncSpec {
	if specs, fresh := p.cachedToolSpecs(); fresh {
		return specs
	}
	// 仅在无等待者且有空闲实例时活体查询(tryAcquire 不入队)。
	if i, err := p.tryAcquire(); err == nil {
		ctx, cancel := context.WithTimeout(withHeld(context.Background(), p), toolSpecsTimeout)
		out, live := json.RawMessage(nil), false
		if i.mod != nil && !i.broken.Load() { // 注册期 mod 未挂:回落快照/清单(装配序守卫)
			if o, err := i.callExport(p, ctx, ExportToolSpecs); err == nil {
				out, live = o, true
			}
		}
		cancel()
		p.release(i)
		if live {
			var specs []contract.FuncSpec
			if json.Unmarshal(out, &specs) == nil {
				p.storeToolSpecs(specs)
				return specs
			}
		}
	}
	return p.staleToolSpecs()
}

// cachedToolSpecs 新鲜快照(TTL 内)。
func (p *Plugin) cachedToolSpecs() ([]contract.FuncSpec, bool) {
	p.toolMu.Lock()
	defer p.toolMu.Unlock()
	if p.toolCache == nil || time.Since(p.toolCacheAt) > toolSpecsTTL {
		return nil, false
	}
	return append([]contract.FuncSpec(nil), p.toolCache...), true
}

// staleToolSpecs 过期快照优先(动态面已知集合优于静态清单),无快照退清单。
func (p *Plugin) staleToolSpecs() []contract.FuncSpec {
	p.toolMu.Lock()
	defer p.toolMu.Unlock()
	if p.toolCache != nil {
		return append([]contract.FuncSpec(nil), p.toolCache...)
	}
	return p.manifest.Tools
}

func (p *Plugin) storeToolSpecs(specs []contract.FuncSpec) {
	p.toolMu.Lock()
	p.toolCache, p.toolCacheAt = specs, time.Now()
	p.toolMu.Unlock()
}

// invalidateToolSpecs 配置变更后强制下次调用重新活体查询(动态工具面可能随
// 连接/发现变化;新的发现窗口 = TTL,不阻塞启动)。
func (p *Plugin) invalidateToolSpecs() {
	p.toolMu.Lock()
	p.toolCacheAt = time.Time{}
	p.toolMu.Unlock()
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
	p.invalidateToolSpecs() // 动态工具面可能随配置变化,下次查询重发现
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
	if ctx == nil {
		ctx = context.Background()
	}
	p.closed.Store(true)
	p.wakeAllWaiters() // 排队者收 nil=关闭错误,绝不陪葬挂死
	var errs []error
	p.poolMu.Lock()
	insts := append([]*inst(nil), p.insts...)
	p.insts = nil
	p.byName = map[string]*inst{}
	p.poolMu.Unlock()

	// 标记实际持锁者的兜底收尾责任。正常路径拿到锁后清除标记;若调用方
	// deadline 先于非协作 host 回调返回,release 会清理,无需另起锁等待 goroutine。
	for _, i := range insts {
		i.closeOnRelease.Store(true)
	}
	forced := false
	for _, i := range insts {
		if !i.mu.TryLock() {
			forced = true
			break
		}
		i.mu.Unlock()
	}
	// 不得在 inst.mu 上先等:纯计算 guest 可能永不返回。关 runtime 会先打断
	// 活跃模块(WithCloseOnContextDone),再进入下面的常规资源清退。
	if forced && p.runtime != nil {
		if err := p.runtime.CloseWithExitCode(ctx, 255); err != nil {
			errs = append(errs, err)
		}
	}
	for _, i := range insts {
		if err := lockInstanceForClose(ctx, i); err != nil {
			errs = append(errs, err)
			continue
		}
		i.closeOnRelease.Store(false)
		_ = i.endKVUpdate()
		if err := i.unsubs.Close(); err != nil {
			errs = append(errs, err)
		}
		if i.mod != nil {
			if !forced && i.mod.ExportedFunction(ExportShutdown) != nil {
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

// lockInstanceForClose 用 TryLock 观察 ctx,不为可能永不释放的 sync.Mutex
// 留下一个锁等待 goroutine。空闲快路径无定时器开销。
func lockInstanceForClose(ctx context.Context, i *inst) error {
	if i.mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if i.mu.TryLock() {
				return nil
			}
		}
	}
}

// Busy 实现 host.Quiescer 可选探测：TryLock 检查
// 全池——任一实例被占=在飞调用存在。只探测不等待不阻塞在飞调用;
// 宿主热更收尾据此决定等待或放弃(host/quiesce.go)。
func (p *Plugin) Busy() (bool, error) {
	if p.closed.Load() {
		return false, nil // 已关=静默
	}
	p.poolMu.Lock()
	insts := append([]*inst(nil), p.insts...)
	p.poolMu.Unlock()
	for _, i := range insts {
		if !i.mu.TryLock() {
			return true, nil
		}
		i.mu.Unlock()
	}
	return false, nil
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
