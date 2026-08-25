// MemHooks 是开箱可用的**内存 hook 总线**:一个 hook 点可有多个回调,触发时
// 全部回调各返回一个 contract.HookResult(回调写 Block/Reason/Data,框架填
// Who/At/Seq)。业务侧可装配注入自己的实现(例如带持久化/跨进程的 hook 总线)。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
)

// Hooks 是 hook 回调总线消费口:On 注册回调(带 owner,供框架回填 Who)、
// Trigger 触发并收集结果。
type Hooks interface {
	On(owner, hookID string, fn contract.HookFunc) func()
	Trigger(ctx context.Context, hookID string, data json.RawMessage) []contract.HookResult
}

type hookEntry struct {
	id    uint64
	owner string
	fn    contract.HookFunc
}

// MemHooks 线程安全的内存 hook 总线(回调在锁外执行;同一 hook 点允许多个
// 回调并存,各自独立撤销)。
type MemHooks struct {
	mu   sync.Mutex
	subs map[string]map[uint64]hookEntry
	next uint64
}

// NewMemHooks 构造空 hook 总线。
func NewMemHooks() *MemHooks {
	return &MemHooks{subs: map[string]map[uint64]hookEntry{}}
}

// On 注册回调,返回撤销函数(幂等;同一 hook 点可多个回调并存)。
func (m *MemHooks) On(owner, hookID string, fn contract.HookFunc) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subs[hookID] == nil {
		m.subs[hookID] = map[uint64]hookEntry{}
	}
	m.next++
	id := m.next
	m.subs[hookID][id] = hookEntry{id: id, owner: owner, fn: fn}
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if set := m.subs[hookID]; set != nil {
			delete(set, id)
			if len(set) == 0 {
				delete(m.subs, hookID)
			}
		}
	}
}

// Trigger 触发 hook:按**注册序**执行回调,框架把 Who/At/Seq 回填进每个 HookResult。
func (m *MemHooks) Trigger(ctx context.Context, hookID string, data json.RawMessage) []contract.HookResult {
	now := time.Now().UnixMilli()
	m.mu.Lock()
	entries := make([]hookEntry, 0, len(m.subs[hookID]))
	for _, e := range m.subs[hookID] {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	out := make([]contract.HookResult, 0, len(entries))
	for i, e := range entries {
		r := safeHookResult(ctx, e.fn, hookID, data)
		// 框架回填执行上下文(回调写入的 Block/Reason/Data 原样保留)。
		r.Origin = &contract.Origin{ID: e.owner, Kind: contract.OriginHook, Point: hookID, At: now}
		r.Seq = i + 1
		out = append(out, r)
	}
	return out
}

// safeHookResult 执行 hook 回调,把 panic 归一成非阻断的 HookResult(机制层 fail-closed:
// hook 回调崩了记 Reason,不炸宿主、不拦后序回调)。
func safeHookResult(ctx context.Context, fn contract.HookFunc, hookID string, data json.RawMessage) (hr contract.HookResult) {
	defer func() {
		if p := recover(); p != nil {
			hr = contract.HookResult{Reason: fmt.Sprintf("hook panic: %v", p)}
		}
	}()
	return contract.HookResultOf(fn(ctx, hookID, data))
}
