// Package config 是配置面契约（框架骨架）。读面直接复用 contract.Source（与宿主
// Options.Settings 同源），写面 Putter 是本地补充（contract 未定义设置写面——
// host 的 MapSettings.Set 是实装）。
package config

import (
	"encoding/json"

	"github.com/ejfkdev/egop/contract"
)

// Source 配置读面 = contract.Source（能力回环与校验面的最小契约）。
type Source = contract.Source

// Putter 配置写面（可选；host 侧实装为 MapSettings.Set）。
type Putter interface {
	Put(key string, v json.RawMessage)
}
