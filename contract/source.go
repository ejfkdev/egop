package contract

import "encoding/json"

// Source 配置读面（能力回环与校验面的最小契约）。
type Source interface {
	Get(key string) (json.RawMessage, bool)
}
