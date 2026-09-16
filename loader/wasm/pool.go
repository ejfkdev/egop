// 实例池:并发模型。
// guest 实例非并发安全,每 inst 一把 mu;跨边界入口(函数/工具)TryLock 空闲
// 实例——并发度由实例数自然约束,无需配额信号量(信号量制曾在 Close/扩容/嵌套
// 三处翻车:release 对已置 nil 的 sem 永挂、growPool 换 sem 破坏配额账、嵌套
// 等待只认 ctx.Done 对 Background 永挂)。
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
	"time"

	"github.com/tetratelabs/wazero/api"
)

// maxPool wasm 实例池上限(内存/装配成本护栏;正常插件 1-2)。
const maxPool = 4

// poolSize 清单扩展 egop.pool 声明的实例池大小(缺省/非法=1;上限 maxPool)。
func (p *Plugin) poolSize() int {
	if raw, ok := p.manifest.Extensions["egop.pool"]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			if n > maxPool {
				n = maxPool
			}
			return n
		}
	}
	return 1
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

// acquire 取一个空闲实例(TryLock)。同调用栈嵌套且池耗尽时立即返回
// "all instances busy"(等待即自死锁);并发调用方继续等(ctx 可取消)。
func (p *Plugin) acquire(ctx context.Context) (*inst, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		// 每轮重取快照:growPool 增补 / Close 摘表对等待者可见。
		p.poolMu.Lock()
		insts := p.insts
		p.poolMu.Unlock()
		if len(insts) == 0 {
			if p.closed.Load() {
				return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
			}
			return nil, fmt.Errorf("wasm plugin %s: codeless bundle has no callable functions", p.name)
		}
		for _, i := range insts {
			if i.mu.TryLock() {
				return i, nil
			}
		}
		if p.closed.Load() {
			return nil, fmt.Errorf("wasm plugin %s: instance closed", p.name)
		}
		if heldBySelf(ctx, p) {
			// 外层持有就在本调用栈上,不会释放——等待即自死锁,立即回落。
			return nil, fmt.Errorf("wasm plugin %s: all instances busy (nested call needs egop.pool>=2)", p.name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// release 释放实例(仅解锁;无配额簿记)。
func (p *Plugin) release(i *inst) { i.mu.Unlock() }

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
	}
}
