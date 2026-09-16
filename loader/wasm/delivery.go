// 事件/hook 投递(宿主→guest):pushEvent/invokeHook。投递钉在**主实例**
// (tryAcquirePrimary):订阅/hook 注册也只挂主实例,观察面语义不随池放大;
// 主实例忙(总线同步扇出可能重入"guest 正持实例锁调用中"的同一 goroutine)
// 即跳过本次投递(事件丢弃/hook 记 Reason),绝不阻塞重入。
package wasm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ejfkdev/egop/contract"
)

// deliveryTimeout 是投递(egop_on_event/egop_on_hook)的兜底时限:挂死的
// guest 处理器经看门狗打断(CloseWithExitCode→broken,下次调用 revive),
// 事件总线的同步扇出 goroutine / TriggerHook 不被永挂。投递是 fire-and-forget:
// 发布者/触发方 ctx 的取消**不传导**(WithoutCancel),只有时限打断。
const deliveryTimeout = 10 * time.Second

// pushEvent 是订阅回调:把事件经 egop_on_event 推给 guest(尽力而为,未导出回调则丢弃)。
// 统一事件结构:整个 contract.Event(含 Source/Labels)JSON 作为单参传给 guest。
// 投递非阻塞锁定主实例:事件总线同步扇出,回调可能落在"guest 自己正持锁调用中"
// 的 goroutine 上(插件发布了命中自身订阅的事件)——主实例忙即本次投递丢弃
// (best-effort 观察面语义;不阻塞总线、不殃及其它订阅者)。
func (p *Plugin) pushEvent(ctx context.Context, _ string, e contract.Event) {
	i := p.tryAcquirePrimary()
	if i == nil {
		return
	}
	defer p.release(i)
	if i.broken.Load() || i.mod == nil {
		return
	}
	if i.mod.ExportedFunction(ExportOnEvent) == nil {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()
	// 事件投递 best-effort:失败(含看门狗打断)静默(观察面不拦不改)。
	_ = i.callVoid(p, dctx, ExportOnEvent, string(raw))
}

// invokeHook 把 hook 触发转发给 guest 的 egop_on_hook 导出,解其返回信封的
// result 得到 HookResult(Block/Reason/Data;Who/At/Seq 由框架回填)。
// 取锁用 tryAcquirePrimary(同 pushEvent:hook 触发可能同步重入持锁中的 guest
// 调用,阻塞即死锁);主实例忙归一为非阻断 HookResult{Reason},触发方继续、
// hook 链不断。看门狗同 callExport:挂死的 handler 按时限打断(broken→revive)。
func (p *Plugin) invokeHook(ctx context.Context, hookID string, data json.RawMessage) any {
	i := p.tryAcquirePrimary()
	if i == nil {
		return contract.HookResult{Reason: "wasm plugin " + p.name + ": instance busy (hook skipped to avoid reentrant deadlock)"}
	}
	defer p.release(i)
	if i.broken.Load() || i.mod == nil {
		return contract.HookResult{Reason: "instance closed"}
	}
	fn := i.mod.ExportedFunction(ExportOnHook)
	if fn == nil {
		return contract.HookResult{}
	}
	args := []string{hookID, string(data)}
	// egop_on_hook 第 3 参=触发来源 Origin(裸 JSON,精确 6 参才传,同 egop_call):
	// SDK guest 读它并 WithOrigin 还原,使 hook 回调也能经 OriginFrom 知道
	// "哪个框架点触发/谁触发"。老 WAT 夹具 4 参则跳过。
	if len(fn.Definition().ParamTypes()) == 6 {
		originJSON, _ := json.Marshal(contract.OriginFrom(ctx))
		args = append(args, string(originJSON))
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()
	out, err := i.callExport(p, dctx, ExportOnHook, args...)
	if err != nil {
		return contract.HookResult{Reason: err.Error()}
	}
	var hr contract.HookResult
	if len(out) > 0 {
		_ = json.Unmarshal(out, &hr)
	}
	return hr
}
