package host

// 生命周期静默分两层：
//   - pluginGeneration 是宿主权威的在飞调用账本。Host.Call 与已收集 Tool.Run
//     进入前取得 lease，Replace/Remove 先封闭该代准入，再等待 lease 归零。
//   - Quiescer 是兼容的可选插件探测面，用来覆盖绕过 Host 的实例内活动；
//     它不是宿主在飞调用计数的替代品。
//
// 等待只约束等待方，不强杀插件调用。超时/取消返回错误，未提交的生命周期
// 事务必须恢复旧代准入，不得关闭旧 effect 或宣称实例已排空。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ejfkdev/egop/contract"
)

// ErrQuiesceTimeout 表示旧 generation 在指定上限内未排空，或兼容
// Quiescer 仍报告 busy。Replace/Remove 返回该错误时不提交目录变更。
var ErrQuiesceTimeout = errors.New("host: plugin generation did not quiesce before deadline")

// Quiescer 可选:插件自报忙闲探测面(宿主热更收尾等待用)。
// 返回 (busy, err):busy=true=仍有在飞调用;err=探测面不可用。
// 宿主始终先等待自己的 generation lease，再使用此探测面。
type Quiescer interface {
	Busy() (bool, error)
}

// pluginGeneration 的字段全部由 Host.mu 保护。drained 仅在 sealed=true
// 时存在，并在 inFlight 归零时关闭；事务失败可 resume，事务提交后永久封闭。
type pluginGeneration struct {
	id        string
	number    uint64
	accepting bool
	sealed    bool
	inFlight  int
	drained   chan struct{}
}

func (h *Host[C]) newGenerationLocked(id string) *pluginGeneration {
	h.nextGeneration++
	return &pluginGeneration{id: id, number: h.nextGeneration, accepting: true}
}

// acquireGenerationLocked 原子完成“仍是当前代 + 允许准入 + lease 计数”。
// 需持 h.mu；调用方成功后必须 releaseGeneration。
func (h *Host[C]) acquireGenerationLocked(g *pluginGeneration) bool {
	if g == nil || !g.accepting || h.generations[g.id] != g {
		return false
	}
	g.inFlight++
	return true
}

func (h *Host[C]) releaseGeneration(g *pluginGeneration) {
	if g == nil {
		return
	}
	h.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	if g.sealed && g.inFlight == 0 && g.drained != nil {
		select {
		case <-g.drained:
		default:
			close(g.drained)
		}
	}
	h.mu.Unlock()
}

// sealGenerationLocked 禁止旧代新调用并返回其 lease 排空通知。需持 h.mu。
func (h *Host[C]) sealGenerationLocked(g *pluginGeneration) <-chan struct{} {
	if g == nil {
		return nil
	}
	if !g.sealed {
		g.accepting = false
		g.sealed = true
		g.drained = make(chan struct{})
		if g.inFlight == 0 {
			close(g.drained)
		}
	}
	return g.drained
}

// resumeGenerationLocked 仅用于尚未提交的失败事务。已替换/删除的代不再
// 是 current generation，因此永远不会被恢复。需持 h.mu。
func (h *Host[C]) resumeGenerationLocked(g *pluginGeneration) {
	if g == nil || h.generations[g.id] != g || !g.sealed {
		return
	}
	g.accepting = true
	g.sealed = false
	g.drained = nil
}

func (h *Host[C]) generationDrained(g *pluginGeneration) bool {
	if g == nil {
		return true
	}
	h.mu.Lock()
	drained := g.inFlight == 0
	h.mu.Unlock()
	return drained
}

func quiesceWaitError(ctx context.Context, pluginID string, g *pluginGeneration) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	if g != nil {
		return fmt.Errorf("host: plugin %s generation %d: %w", pluginID, g.number, ErrQuiesceTimeout)
	}
	return fmt.Errorf("host: plugin %s: %w", pluginID, ErrQuiesceTimeout)
}

// awaitGenerationQuiesce 等待指定旧代的宿主 lease 与兼容 Quiescer 都静默。
// g 必须已由调用方 seal；wait<=0 只做一次非阻塞检查，忙时返回
// ErrQuiesceTimeout，绝不把“未等待”误报成“已排空”。
func (h *Host[C]) awaitGenerationQuiesce(ctx context.Context, pluginID string, g *pluginGeneration, p contract.Plugin, wait time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return quiesceWaitError(ctx, pluginID, g)
	}
	if wait <= 0 {
		if !h.generationDrained(g) {
			return quiesceWaitError(ctx, pluginID, g)
		}
		// 旧 Replace/Remove 默认零等待不调用 Busy 探测。仅校验宿主 generation
		// lease，避免把“实现了 Quiescer 但探测不可用”误当成替换失败。
		return nil
	}

	dctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	h.mu.Lock()
	drained := g == nil || g.inFlight == 0
	var drainedCh <-chan struct{}
	if g != nil {
		drainedCh = g.drained
	}
	h.mu.Unlock()
	if !drained {
		select {
		case <-drainedCh:
		case <-dctx.Done():
			return quiesceWaitError(dctx, pluginID, g)
		}
	}

	q, ok := p.(Quiescer)
	if !ok {
		return nil
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		busy, err := q.Busy()
		if err != nil {
			// Quiescer 只是兼容增强面；宿主 generation lease 已提供真实
			// 排空保证，探测不可用按无探测面降级，不阻断 Replace/Remove。
			h.logf("host: plugin %s quiesce probe unavailable: %v", pluginID, err)
			return nil
		}
		if !busy {
			return nil
		}
		select {
		case <-dctx.Done():
			return quiesceWaitError(dctx, pluginID, g)
		case <-tick.C:
		}
	}
}

// awaitQuiesce 保留原有只读探测辅助语义，供包内诊断/兼容测试使用。
// 生命周期提交路径使用 awaitGenerationQuiesce，而不是按 id 再查当前实例。
func (h *Host[C]) awaitQuiesce(ctx context.Context, pluginID string, wait time.Duration) (bool, error) {
	if wait <= 0 {
		return false, nil
	}
	h.mu.Lock()
	p, ok := h.plugins[pluginID]
	h.mu.Unlock()
	if !ok {
		return true, nil // 已不在册=静默
	}
	return h.awaitQuiesceInstance(ctx, pluginID, p, wait)
}

// awaitQuiesceInstance 是旧 Quiescer 等待辅助。Replace 必须走显式旧
// generation 的 awaitGenerationQuiesce，避免目录切换后误探测新实例。
func (h *Host[C]) awaitQuiesceInstance(ctx context.Context, pluginID string, p contract.Plugin, wait time.Duration) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if wait <= 0 {
		return false, nil
	}
	q, ok := p.(Quiescer)
	if !ok {
		return false, nil // 兼容旧探测语义：无 Quiescer 时不等待
	}
	dctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		busy, err := q.Busy()
		if err != nil {
			return false, fmt.Errorf("quiesce probe %s: %w", pluginID, err)
		}
		if !busy {
			return true, nil
		}
		select {
		case <-dctx.Done():
			return false, nil // 只放弃等待方，不宣称实例已停
		case <-tick.C:
		}
	}
}
