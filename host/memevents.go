// MemEvents 是开箱可用的**内存事件总线**:Options.Events 缺省即挂它——
// 通用库使用者零实现即可获得事件订阅/广播面;业务侧仍可装配注入自己的总线
// (例如带持久化/跨进程扇出的实现)。
package host

import (
	"context"
	"sort"
	"sync"

	"github.com/ejfkdev/egop/contract"
)

// subEntry 是一个订阅者:过滤条件 + 回调。
type subEntry struct {
	id     uint64
	filter contract.EventFilter
	fn     func(context.Context, contract.Event)
}

// MemEvents 线程安全的内存总线(同步扇出:回调内不重入 Dispatch,避免锁递归)。
// 订阅按 EventFilter 匹配(空 filter = 命中一切),同一过滤条件允许多订阅者并存、
// 各自独立撤销。
type MemEvents struct {
	mu     sync.Mutex
	subs   map[uint64]subEntry
	topics map[string]bool
	next   uint64
}

// NewMemEvents 构造空总线。
func NewMemEvents() *MemEvents {
	return &MemEvents{
		subs:   map[uint64]subEntry{},
		topics: map[string]bool{},
	}
}

// Subscribe 按过滤条件订阅(nil 或零值 = 命中一切),返回撤销函数(幂等)。
// 内部复制 filter,订阅后调用方再改动原 filter 不影响已建立的订阅。
func (m *MemEvents) Subscribe(f *contract.EventFilter, fn func(context.Context, contract.Event)) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := m.next
	var filter contract.EventFilter
	if f != nil {
		filter = *f
	}
	m.subs[id] = subEntry{id: id, filter: filter, fn: fn}
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.subs, id)
	}
}

// Dispatch 广播事件:对每个订阅者做 filter.Match,命中即回调。订阅者按注册序、
// 在锁外回调(避免回调内再订阅/发布死锁)。
func (m *MemEvents) Dispatch(ctx context.Context, e contract.Event) {
	m.mu.Lock()
	entries := make([]subEntry, 0, len(m.subs))
	for _, en := range m.subs {
		entries = append(entries, en)
	}
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	for _, en := range entries {
		if en.filter.Match(e) {
			en.fn(ctx, e)
		}
	}
}

// EnsureTopic 落主题位(声明面;幂等)。Topics 返回已确保的主题集。
func (m *MemEvents) EnsureTopic(topic string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.topics[topic] = true
}

// Topics 返回已注册/确保的主题名快照。
func (m *MemEvents) Topics() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.topics))
	for t := range m.topics {
		out = append(out, t)
	}
	return out
}
