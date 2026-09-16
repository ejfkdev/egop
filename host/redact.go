package host

import (
	"encoding/json"
	"strings"
)

// 敏感字段名模式(小写子串命中即脱敏)。纯 JSON 形状词汇,不含业务语义——
// 内容无关宿主对"配置观察事件可能带密钥"的机制级兜底(ADR:密钥不进日志)。
var sensitiveKeyParts = []string{
	"token", "secret", "password", "passwd", "api_key", "apikey",
	"access_key", "private_key", "credential", "authorization",
}

const redactedMark = "***redacted***"

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, p := range sensitiveKeyParts {
		if strings.Contains(lk, p) {
			return true
		}
	}
	return false
}

// redactJSON 键名启发式脱敏(无声明时的兜底路径)。
func redactJSON(raw json.RawMessage) json.RawMessage {
	return redactJSONWith(raw, nil)
}

// redactJSONWith 声明优先的脱敏:json 对象**顶层**命中 ConfigFieldSpec.Secret
// 声明的键强制遮(键名不敏感也遮——声明真源),其余层(含嵌套/数组)仍走键名
// 子串启发。解析失败(非 JSON 对象/数组)原样返回——观察事件宁可原样也不丢。
// 只用于**事件 payload 投影**;宿主内部 applied 配置保留全值(热更回灌需要)。
func redactJSONWith(raw json.RawMessage, secret map[string]bool) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(redactValue(v, secret, true))
	if err != nil {
		return raw
	}
	return out
}

func redactValue(v any, secret map[string]bool, top bool) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitiveKey(k) || (top && secret[k]) {
				if s, ok := val.(string); ok && s == "" {
					m[k] = "" // 空值不遮(三态语义:nil/""/值)
					continue
				}
				m[k] = redactedMark
				continue
			}
			m[k] = redactValue(val, secret, false)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val, secret, false)
		}
		return out
	default:
		return v
	}
}
