// revive:意外打断(ctx 取消看门狗/runtime trap)后的单实例自愈——重建 module
// (复用 compiled)、egop_init 重放、最近配置回放;显式 Close 是终态不复活。
package wasm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/undo"
)

// reviveLocked 在意外打断(ctx 取消看门狗/runtime trap)后重建单个 guest 实例(须持该 inst mu)。
// 语义 = 对该实例做一次热重启:旧订阅撤销、module 关闭重建(复用 compiled)、egop_init
// 重放(guest 在 init 里重挂事件订阅)、最近生效配置回放;插件内存态归零(KV 等宿主侧
// 持久态不受影响)。显式 Close 与无代码包不复活;重建失败保持 broken(下次调用再试)。
func (p *Plugin) reviveLocked(ctx context.Context, i *inst) error {
	if p.closed.Load() {
		return fmt.Errorf("wasm plugin %s: instance closed", p.name)
	}
	// 竞争收口:compiled/lastCfg 在 poolMu 下读写(Close/ApplyConfig 写同锁),
	// 快照后使用——revive 与 Close/ApplyConfig 并发也不读写撕裂。
	if p.runtime == nil {
		return fmt.Errorf("wasm plugin %s: codeless bundle has no callable functions", p.name)
	}
	compiled := p.compiledModule()
	if compiled == nil {
		return fmt.Errorf("wasm plugin %s: codeless bundle has no callable functions", p.name)
	}
	p.poolMu.Lock()
	lastCfg := append(json.RawMessage(nil), p.lastCfg...)
	p.poolMu.Unlock()
	surf := p.surf()
	if p.logFn != nil {
		p.logFn("warn", "reviving wasm instance after interruption")
	}
	_ = i.unsubs.Close()
	i.unsubs = undo.Catcher{}
	if i.mod != nil {
		_ = i.mod.Close(context.WithoutCancel(ctx))
		i.mod = nil
	}
	i.netCloseAll()
	mod, err := p.instantiateOne(context.WithoutCancel(ctx), compiled, i.name)
	if err != nil {
		i.broken.Store(true)
		return fmt.Errorf("wasm plugin %s: revive: %w", p.name, err)
	}
	i.mod = mod
	i.broken.Store(false)
	// 回放 init(surface 不变;guest 在 init 内重建自己的订阅/内部状态)。
	if surf != nil && mod.ExportedFunction(ExportInit) != nil {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), initTimeout)
		defer cancel()
		if _, err := i.callExport(p, ictx, ExportInit); err != nil {
			i.broken.Store(true)
			return fmt.Errorf("wasm plugin %s: revive: init: %w", p.name, err)
		}
	}
	// 回放最近配置(失败非致命:实例已可用,宿主仍可重新 SetConfig)。
	if len(lastCfg) > 0 && mod.ExportedFunction(ExportApplyConfig) != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyConfigTimeout)
		defer cancel()
		if _, err := i.callExport(p, cctx, ExportApplyConfig, string(lastCfg)); err != nil {
			return fmt.Errorf("wasm plugin %s: revive: replay config: %w", p.name, err)
		}
	}
	return nil
}
