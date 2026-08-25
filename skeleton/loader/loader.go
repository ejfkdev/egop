// Package loader 是组件装载表骨架：类型化服务令牌 Service[T] 派生键，
// Provide/Resolve/Must 编译期契约取件。
// 这是**通用 DI/组件表原语**，与 contract 的插件契约(Meta/Slot/函数面)是两层；
// 插件生命周期本体见 core/host，本包仅演示"类型化取件"这一种骨架写法。
package loader

import (
	"fmt"
	"reflect"
	"sync"
)

// Service 类型化服务令牌形态（键 = "pkgpath.Type" 稳定名）。
type Service[T any] string

// Key 派生契约类型 T 的服务令牌（指针归一）。
func Key[T any]() Service[T] {
	var zero T
	t := reflect.TypeOf(&zero).Elem()
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return Service[T](t.PkgPath() + "." + t.Name())
}

// Registry 组件装载表。
type Registry interface {
	Register(id string, v any) error
	Replace(id string, v any) error
	Remove(id string)
	Get(id string) (any, bool)
	IDs() []string
}

// Map 是装载表默认实装（sync.Map 保序版本:pkg 级最小,业务层可替换）。
type Map struct {
	mu    sync.Mutex
	items map[string]any
	order []string
}

func NewMap() *Map { return &Map{items: map[string]any{}} }

func (m *Map) Register(id string, v any) error {
	if id == "" {
		return fmt.Errorf("loader: empty id rejected")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		m.order = append(m.order, id)
	}
	m.items[id] = v
	return nil
}

func (m *Map) Replace(id string, v any) error { return m.Register(id, v) }
func (m *Map) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	for i, k := range m.order {
		if k == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}
func (m *Map) Get(id string) (any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	return v, ok
}
func (m *Map) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// Provide 按契约类型注册组件（键 = Key[T]）。
func Provide[T any](r Registry, v T) error {
	return r.Register(string(Key[T]()), v)
}

// Resolve 按契约类型取件。
func Resolve[V any](r Registry) (V, bool) {
	return Get[V](r, string(Key[V]()))
}

// ResolveMust 缺件即 panic（装配期专用）。
func ResolveMust[V any](r Registry) V {
	return Must[V](r, string(Key[V]()))
}

// Get 按 id 取件并断言类型。
func Get[V any](r Registry, id string) (V, bool) {
	raw, ok := r.Get(id)
	if !ok || raw == nil {
		var zero V
		return zero, false
	}
	v, ok := raw.(V)
	return v, ok
}

// Must 按 id 取件。
func Must[V any](r Registry, id string) V {
	v, ok := Get[V](r, id)
	if !ok {
		panic(fmt.Sprintf("loader: component %q missing (type %T)", id, *new(V)))
	}
	return v
}
