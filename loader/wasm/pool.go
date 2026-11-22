// 实例池:并发模型。
// guest 实例非并发安全,每 inst 一把 mu;跨边界入口(函数/工具)取空闲实例
// ——并发度由实例数自然约束,无需配额信号量(信号量制曾在 Close/扩容/嵌套
// 三处翻车:release 对已置 nil 的 sem 永挂、growPool 换 sem 破坏配额账、嵌套
// 等待只认 ctx.Done 对 Background 永挂)。
// **公平性(2026-10-15 eha 跑分实锤第四课)**:纯 TryLock 自旋无 FIFO——实例
// 被长阻塞轮次(前台 nmap/sleep 整轮持有)占住时,新到者可以无限插队,先到的
// 等待者**无限期饥饿**(协调者 agent 35 分钟拿不到 react 实例,而后来者持续
// 抢走)。改为:无等待者时 TryLock 快路径;有等待者时新到者一律排队尾,释放
// 走**所有权交接**(实例锁不放开,直接递给队首)——严格 FIFO,零插队。
// 嵌套重入(heldBySelf)永不排队(等待即自死锁,立即 busy 报错由调用方回落);
// Close 唤醒全部等待者(收 nil=关闭错误);growPool 增补后向队首献新实例。
// 锁序恒 waitMu→poolMu(release/eachInst/growPool 只取单边,无反向)。
// 同 goroutine 嵌套重入(宿主注入函数回调宿主、宿主再进同一插件)经 ctx 重入
// 标记识别:有其它空闲实例即取第二实例(池≥2 的嵌套正解);池耗尽**立即**返回
// busy 错误(调用方回落)——此时等待即自死锁,外层要等嵌套返回才释放。
// 事件/hook 投递与订阅钉在主实例(insts[0]):池>1 时观察面语义不随投递目标漂移。
package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// 实例池上限不再是一个固定常数。原 `maxPool = 8` 是"内存/装配成本护栏",
// 但实测(2026-11-09,react 包 pool=1/8/16)空闲实例每个只多占 ~0.3MB:guest
// 线性内存按需增长,`egop.memory_pages` 只是**上限**不是预留。固定 8 反而成了
// 运行质量的瓶颈——eha 跑分时 3 解题者 + 协调者 + 退役尾巴就能占满,新 agent
// 拿不到实例后静默排队数十分钟。
// 现在改为按主机资源自适应:池上限 = min(absPoolCeil, 主机内存×poolMemFrac /
// 单实例最坏内存)。空闲不花钱、最坏情况不炸机器;`EGOP_POOL_CEIL` 可强制覆盖。
const (
	// absPoolCeil 绝对上限(防呆):再大的机器也不开几百个 guest。
	absPoolCeil = 256
	// poolMemFrac 允许实例"同时涨到上限"的内存占比(按单插件算)。0.35 意味着
	// 48GB 机器上 react(单实例上限 512MiB)拿 33 槽≈最坏 16.5GB,仍留 65% 给宿主;
	// 实际并发受 agent 数约束,槽位基本不会同时涨满。
	poolMemFrac = 0.35
)

// poolCeiling 本插件允许的池上限(按单实例最坏内存折算主机内存)。
func (p *Plugin) poolCeiling() int {
	if v := envPoolCeil(); v > 0 {
		return v
	}
	per := p.instMaxBytes()
	mem := hostMemoryBytes()
	if per == 0 || mem == 0 {
		// 内存未知(非 linux/darwin):退回 CPU 规模,别再用 8 卡死并发。
		n := 4 * runtime.NumCPU()
		if n < 8 {
			n = 8
		}
		if n > absPoolCeil {
			n = absPoolCeil
		}
		return n
	}
	n := int(float64(mem) * poolMemFrac / float64(per))
	if n < 8 {
		n = 8
	}
	if n > absPoolCeil {
		n = absPoolCeil
	}
	return n
}

// instMaxBytes 单实例最坏内存(清单 egop.memory_pages,缺省 DefaultMaxPages)。
func (p *Plugin) instMaxBytes() uint64 {
	pages := DefaultMaxPages
	if raw, ok := p.manifest.Extensions["egop.memory_pages"]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			pages = uint32(n)
		}
	}
	if pages > MaxPagesCeiling {
		pages = MaxPagesCeiling
	}
	return uint64(pages) * 64 * 1024
}

// envPoolCeil EGOP_POOL_CEIL 覆盖(<=0/非法=未设)。
func envPoolCeil() int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EGOP_POOL_CEIL")))
	if err != nil || v <= 0 {
		return 0
	}
	if v > absPoolCeil {
		v = absPoolCeil
	}
	return v
}

// poolMax 清单扩展 egop.pool_max 的每插件硬上限(0=不设):
// 主机内存很大时 auto 会铺得很宽(48GB → react 33),但实际并发由 agent 数决定;
// 给个"够用即可"的帽子能把最坏内存砍半(实例按需增长,帽小也不会饿死正常并发)。
func (p *Plugin) poolMax() int {
	raw, ok := p.manifest.Extensions["egop.pool_max"]
	if !ok {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if n > absPoolCeil {
			n = absPoolCeil
		}
		return n
	}
	return 0
}

// capPool 应用 pool_max(声明/auto 之后)。
func (p *Plugin) capPool(n int) int {
	if m := p.poolMax(); m > 0 && n > m {
		return m
	}
	return n
}

// poolSize 清单扩展 egop.pool 声明的实例池大小:
//   - 整数:按声明,但不超过 poolCeiling();
//   - "auto":直接取 poolCeiling()(按主机资源铺满);
//   - 缺省/非法:1(保持状态型插件单实例语义)。
func (p *Plugin) poolSize() int {
	raw, ok := p.manifest.Extensions["egop.pool"]
	if !ok {
		return 1
	}
	var n int
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if ceil := p.poolCeiling(); n > ceil {
			n = ceil
		}
		return p.capPool(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.EqualFold(strings.TrimSpace(s), "auto") {
		return p.capPool(p.poolCeiling())
	}
	return 1
}

// memoryPages 解析实例内存上限:清单扩展 egop.memory_pages(页,64KiB)可**放大**
// 到 MaxPagesCeiling;opts 显式值作为基线,清单更大时取清单(重插件声明额度)。
func (p *Plugin) memoryPages(opts Options) uint32 {
	lim := opts.MaxMemoryPages
	if lim == 0 {
		lim = DefaultMaxPages
	}
	if raw, ok := p.manifest.Extensions["egop.memory_pages"]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 && uint32(n) > lim {
			lim = uint32(n)
		}
	}
	if lim > MaxPagesCeiling {
		lim = MaxPagesCeiling
	}
	return lim
}

// instForModule 宿主注入函数经调用方 module 认回本 inst(未知名回落主实例)。
func (p *Plugin) instForModule(m api.Module) *inst {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	if i, ok := p.byName[m.Name()]; ok {
		return i
	}
	if len(p.insts) > 0 {
		return p.insts[0]
	}
	return nil
}

// primary 主实例(订阅/hook 注册与事件投递的钉扎点)。无实例(无代码包)返回 nil。
func (p *Plugin) primary() *inst {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	if len(p.insts) > 0 {
		return p.insts[0]
	}
	return nil
}

// registerOnPrimary 判定宿主注入函数的调用方是否主实例(订阅/hook 注册钉主实例:
// 池>1 时次实例的声明按无操作忽略——否则每实例一份订阅=事件扇出 N 次、且投递
// 落点随机漂移)。
func (p *Plugin) registerOnPrimary(m api.Module) bool {
	i := p.instForModule(m)
	prim := p.primary()
	return i != nil && prim != nil && i == prim
}

// acqKey 是 ctx 里的"本插件持有中"标记(值 *Plugin):acquire 成功后注入,
// 经 wazero ctx 透传给宿主注入函数——嵌套 acquire 据此识别同调用栈重入。
type acqKey struct{}

// withHeld 把"本插件实例持有中"写进 ctx(供嵌套重入识别)。
func withHeld(ctx context.Context, p *Plugin) context.Context {
	return context.WithValue(ctx, acqKey{}, p)
}

// heldBySelf 判定 ctx 是否来自本插件的外层持有(同一调用栈)。
func heldBySelf(ctx context.Context, p *Plugin) bool {
	return ctx != nil && ctx.Value(acqKey{}) == p
}

// acquire 取一个空闲实例。无等待者=TryLock 快路径;有等待者=排队尾(FIFO
// 公平,防长阻塞轮次下的无限插队饥饿),释放方经所有权交接直接递给队首。
// 同调用栈嵌套(heldBySelf)永不排队:池耗尽立即返回 "all instances busy"
// (等待即自死锁,调用方回落内嵌 runner)。ctx 可取消;Close 唤醒收 nil。
func (p *Plugin) acquire(ctx context.Context) (*inst, error) {
	closedErr := func() error { return fmt.Errorf("wasm plugin %s: instance closed", p.name) }
	if p.closed.Load() {
		return nil, closedErr()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	nested := heldBySelf(ctx, p)
	for {
		p.waitMu.Lock()
		if p.closed.Load() {
			p.waitMu.Unlock()
			return nil, closedErr()
		}
		if len(p.instsSnapshot()) == 0 {
			p.waitMu.Unlock()
			return nil, fmt.Errorf("wasm plugin %s: codeless bundle has no callable functions", p.name)
		}
		if nested {
			// 外层持有就在本调用栈上,不会释放——排队即自死锁,立即回落。
			i := p.tryAnyLocked()
			p.waitMu.Unlock()
			if i == nil {
				return nil, fmt.Errorf("wasm plugin %s: all instances busy (nested call needs egop.pool>=2)", p.name)
			}
			return i, nil
		}
		if len(p.waiters) == 0 {
			if i := p.tryAnyLocked(); i != nil {
				p.waitMu.Unlock()
				return i, nil
			}
		}
		// 无空闲(或已有人排队)=入队尾,等交接/Close/ctx 取消。
		ch := make(chan *inst, 1)
		p.waiters = append(p.waiters, ch)
		// 池饿死观测(2026-11-09 eha bench9 实锤):池<并发整轮数时新调用者
		// 静默排队可达数十分钟,外部只看到「agent 不进首轮、零 LLM 调用」,
		// 没有一行日志可归因。等待超 30s 打一行 stderr(每次 acquire 至多一条)。
		warn := time.AfterFunc(30*time.Second, func() {
			fmt.Fprintf(os.Stderr, "%s poolstarved: wasm plugin %s 等实例超 30s(pool=%d,排队中)——并发整轮数超过 egop.pool,或长阻塞轮次/退役会话占槽\n",
				time.Now().Format("2006/01/02 15:04:05"), p.name, p.poolSize())
		})
		defer warn.Stop()
		p.waitMu.Unlock()
		select {
		case i := <-ch:
			if i == nil { // Close 唤醒
				return nil, closedErr()
			}
			return i, nil
		case <-ctx.Done():
			p.cancelWaiter(ch)
			if p.closed.Load() {
				return nil, closedErr()
			}
			return nil, ctx.Err()
		}
	}
}

// cancelWaiter 取消排队。若 release 已完成所有权交接,必须经完整 release
// 路径归还实例,让下一位等待者接手,并保留关闭时的收尾语义。
func (p *Plugin) cancelWaiter(ch chan *inst) {
	p.removeWaiter(ch)
	select {
	case i := <-ch:
		if i != nil {
			p.release(i)
		}
	default:
	}
}

// instsSnapshot 池快照(锁序恒 waitMu→poolMu,可在 waitMu 内调用)。
func (p *Plugin) instsSnapshot() []*inst {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	return append([]*inst(nil), p.insts...)
}

// tryAnyLocked TryLock 扫一遍池(调用方持 waitMu:与 release 交接互斥,
// 保证「有空闲却在排队」不共存)。
func (p *Plugin) tryAnyLocked() *inst {
	for _, i := range p.instsSnapshot() {
		if i.mu.TryLock() {
			return i
		}
	}
	return nil
}

// tryAcquire 非阻塞取空闲实例(元数据查询专用):**绝不入队**——有人在等时
// 不插队(公平),无空闲即忙错。元数据不得排在真实工作后面,也不得因长调用
// 烧满等待超时(2026-10-31:ToolSpecs 曾因此每次 agent 启动被拖 10s×忙插件数)。
func (p *Plugin) tryAcquire() (*inst, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	if p.closed.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	if len(p.instsSnapshot()) == 0 {
		return nil, fmt.Errorf("wasm plugin %s: codeless bundle has no callable functions", p.name)
	}
	if len(p.waiters) > 0 {
		return nil, fmt.Errorf("wasm plugin %s: instance busy (waiters queued)", p.name)
	}
	if i := p.tryAnyLocked(); i != nil {
		return i, nil
	}
	return nil, fmt.Errorf("wasm plugin %s: all instances busy", p.name)
}

// removeWaiter 从队列摘除自己(ctx 取消路径;已被弹出=no-op)。
func (p *Plugin) removeWaiter(ch chan *inst) {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	for k, w := range p.waiters {
		if w == ch {
			p.waiters = append(p.waiters[:k], p.waiters[k+1:]...)
			return
		}
	}
}

// wakeAllWaiters Close 唤醒:全部收 nil(关闭错误),队列清空。
func (p *Plugin) wakeAllWaiters() {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	for _, ch := range p.waiters {
		ch <- nil // 缓冲 1,恒不阻塞
	}
	p.waiters = nil
}

// offerFree 向队首献一个空闲实例(growPool 增补后调用;有等待者而新实例闲置
// =不公平)。无等待者或无空闲=no-op。
func (p *Plugin) offerFree() {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	for len(p.waiters) > 0 {
		i := p.tryAnyLocked()
		if i == nil {
			return
		}
		ch := p.waiters[0]
		p.waiters = p.waiters[1:]
		ch <- i // 所有权交接:锁不放开
	}
}

// release 释放实例:有等待者=所有权交接(mu 保持锁定直接递给队首,严格
// FIFO);无等待者=解锁。取消竞态由 removeWaiter 的排水分支兜住(交接在途
// 的实例必被收下还锁),不吞锁。
func (p *Plugin) release(i *inst) {
	if p.closed.Load() && i.closeOnRelease.Swap(false) {
		// Close 已强制关 runtime,但调用方 ctx 到期时可能仍未拿到 mu。
		// 在实际持锁者的返回路径收尾,避免留下订阅与流式响应资源。
		_ = i.unsubs.Close()
		i.netCloseAll()
		i.broken.Store(true)
	}
	p.waitMu.Lock()
	for len(p.waiters) > 0 {
		ch := p.waiters[0]
		p.waiters = p.waiters[1:]
		select {
		case ch <- i:
			p.waitMu.Unlock()
			return
		default: // 理论不达(缓冲 1 且每 ch 只被弹一次);保底换下一个
		}
	}
	p.waitMu.Unlock()
	i.mu.Unlock()
}

// eachInst 对所有 inst 串行执行 fn(配置下发/关闭/初始化等全池操作)。
func (p *Plugin) eachInst(fn func(i *inst) error) error {
	p.poolMu.Lock()
	insts := append([]*inst(nil), p.insts...)
	p.poolMu.Unlock()
	var errs []error
	for _, i := range insts {
		i.mu.Lock()
		if err := fn(i); err != nil {
			errs = append(errs, err)
		}
		i.mu.Unlock()
	}
	return errors.Join(errs...)
}

// tryAcquirePrimary 非阻塞锁定主实例(事件/hook 投递专用;主实例忙=未送达)。
func (p *Plugin) tryAcquirePrimary() *inst {
	i := p.primary()
	if i == nil {
		return nil
	}
	if i.mu.TryLock() {
		return i
	}
	return nil
}

// growPool 增补实例至 want 个(裸 wasm 形态 manifest 后知后觉时按清单补池)。
// 实例化在 poolMu 外(wazero 启动函数可回调宿主注入,须防 poolMu 重入);新实例
// **先持锁入池再初始化**(入池即对 acquire 可见,锁保证初始化完成前无人可用);
// p.surface 已接线时(注册后增补)补跑 egop_init,失败置 broken(下次调用 revive
// 再试)。装载路径单 goroutine 调用(LoadFS),并发调用未定义;失败时已建实例
// 保留,调用方 Close 兜底回收。
func (p *Plugin) growPool(ctx context.Context, want int) error {
	for {
		p.poolMu.Lock()
		have := len(p.insts)
		if have >= want {
			p.poolMu.Unlock()
			return nil
		}
		name := fmt.Sprintf("%s#%d", p.name, have)
		p.poolMu.Unlock()
		i := newInst(name)
		mod, err := p.instantiateOne(ctx, p.compiledModule(), name)
		if err != nil {
			return err
		}
		i.mod = mod
		i.mu.Lock()
		p.poolMu.Lock()
		p.insts = append(p.insts, i)
		p.byName[name] = i
		p.poolMu.Unlock()
		if p.surf() != nil && mod.ExportedFunction(ExportInit) != nil {
			ictx, cancel := context.WithTimeout(ctx, initTimeout)
			_, ierr := i.callExport(p, ictx, ExportInit)
			cancel()
			if ierr != nil {
				i.broken.Store(true)
				i.mu.Unlock()
				return fmt.Errorf("wasm plugin %s: grow: init %s: %w", p.name, name, ierr)
			}
		}
		i.mu.Unlock()
		p.offerFree() // 增补即公平:有等待者时新实例直接递队首,不闲置
	}
}
