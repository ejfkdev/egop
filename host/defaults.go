// 开箱即用的默认装配件:settings 存储、点位总线(Options 缺省即挂,零值可用)。
package host

import (
	"encoding/json"
	"sync"
)

// MapSettings 是缺省 settings 存储(内存 map,线程安全)。装配层可先 New 宿主
// 再 Set;未 Set 的键一律 not-found(与 nil 语义一致,但无 nil 恐慌)。
type MapSettings struct {
	mu sync.RWMutex
	m  map[string]json.RawMessage
}

// NewMapSettings 构造空存储。
func NewMapSettings() *MapSettings { return &MapSettings{m: map[string]json.RawMessage{}} }

// Get 读键(键不存在返回 false)。
func (s *MapSettings) Get(key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

// Set 写键。
func (s *MapSettings) Set(key string, v json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = v
}

// Keys 键快照(未排序)。
func (s *MapSettings) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	return out
}

// MemPoints 是缺省点位总线(内存集合,线程安全):EnsurePoint 即可观测
// (控制面查询),零实现成本。
type MemPoints struct {
	mu sync.Mutex
	m  map[string]bool
}

// NewMemPoints 构造空点位集。
func NewMemPoints() *MemPoints { return &MemPoints{m: map[string]bool{}} }

// EnsurePoint 落点(幂等)。
func (p *MemPoints) EnsurePoint(point string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[point] = true
}

// Points 点位快照(未排序)。
func (p *MemPoints) Points() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.m))
	for pt := range p.m {
		out = append(out, pt)
	}
	return out
}
