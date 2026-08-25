// Package registry 是通用命名注册器原语（框架骨架）。这是泛型 K→V 注册表，与
// contract 的插件注册(按 Meta.ID)是两层；插件注册本体见 core/host。
package registry

type Registerer[K comparable, V any] interface {
	Register(v V) error
	Unregister(k K)
	Get(k K) (V, bool)
	Names() []K
}
