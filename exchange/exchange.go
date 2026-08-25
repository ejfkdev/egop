// Package exchange 是交换信封的翻译表机制（Register/NewEvent/Decode/DecodeAs）。
package exchange

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
)

func NewEvent(point string, payload any, subTypeHint string) contract.Event {
	if subTypeHint == "" && payload != nil {
		if t := reflect.TypeOf(payload); t != nil {
			// 与 Register 同款解指针:注册名是"去指针后"的类型名,否则 &Foo{} 会
			// 落到 "*pkg.Foo" 而 Decode 查不到("Foo")。
			for t.Kind() == reflect.Ptr {
				t = t.Elem()
			}
			subTypeHint = t.Name()
			if subTypeHint == "" {
				subTypeHint = t.String()
			}
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = json.RawMessage("null")
	}
	return contract.Event{
		Type:    point,
		SubType: subTypeHint,
		Version: contract.EnvelopeVersion,
		Source:  &contract.Origin{Kind: contract.OriginHost, Point: point, At: time.Now().UnixMilli()},
		Payload: raw,
	}
}

func DecodeAs[T any](e contract.Event) (T, error) {
	var v T
	err := json.Unmarshal(e.Payload, &v)
	return v, err
}

var (
	regMu sync.RWMutex
	regBy = map[string]reflect.Type{}
)

func Register(name string, proto any) {
	t := reflect.TypeOf(proto)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	regMu.Lock()
	defer regMu.Unlock()
	if old, ok := regBy[name]; ok {
		if old == t {
			return
		}
		panic(fmt.Sprintf("exchange: payload name %q registered by both %v and %v", name, old, t))
	}
	regBy[name] = t
}

func Decode(e contract.Event) (any, error) {
	regMu.RLock()
	t, ok := regBy[e.SubType]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("exchange: payload subtype %q not registered", e.SubType)
	}
	v := reflect.New(t).Interface()
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return nil, fmt.Errorf("exchange: decode %q: %w", e.SubType, err)
	}
	return reflect.ValueOf(v).Elem().Interface(), nil
}
