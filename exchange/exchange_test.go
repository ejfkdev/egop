package exchange

import (
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
)

type evtPayload struct {
	N int    `json:"n"`
	S string `json:"s"`
}

func TestNewEventPointerPayloadDecodes(t *testing.T) {
	// 回归:Register 注册的是"去指针后"的类型名(evtPayload),NewEvent 传指针 &evtPayload{}
	// 时必须同样解指针,否则 SubType 变成 "*pkg.evtPayload" 而 Decode 查不到。
	Register("evtPayload", evtPayload{})
	defer func() { regMu.Lock(); delete(regBy, "evtPayload"); regMu.Unlock() }()

	e := NewEvent("topic.a", &evtPayload{N: 7, S: "x"}, "")
	if e.SubType != "evtPayload" {
		t.Fatalf("SubType = %q, want %q (pointer must be deref'd)", e.SubType, "evtPayload")
	}
	got, err := Decode(e)
	if err != nil {
		t.Fatalf("Decode pointer payload: %v", err)
	}
	p, ok := got.(evtPayload)
	if !ok || p.N != 7 || p.S != "x" {
		t.Fatalf("decoded = %#v", got)
	}
}

func TestNewEventValuePayloadDecodes(t *testing.T) {
	Register("evtPayload", evtPayload{})
	defer func() { regMu.Lock(); delete(regBy, "evtPayload"); regMu.Unlock() }()

	e := NewEvent("topic.b", evtPayload{N: 3}, "")
	if _, err := Decode(e); err != nil {
		t.Fatalf("Decode value payload: %v", err)
	}
}

func TestNewEventHostSource(t *testing.T) {
	e := NewEvent("p", nil, "")
	if e.Source == nil || e.Source.Kind != contract.OriginHost || e.Source.Point != "p" {
		t.Fatalf("host source = %+v", e.Source)
	}
	if e.Source.At <= 0 || e.Source.At > time.Now().UnixMilli() {
		t.Fatalf("source At out of range: %d", e.Source.At)
	}
}

func TestNewEventExplicitSubType(t *testing.T) {
	// 显式 subTypeHint 优先于类型推导:注册名与 hint 一致即可解码。
	Register("my_alias", evtPayload{})
	defer func() { regMu.Lock(); delete(regBy, "my_alias"); regMu.Unlock() }()

	e := NewEvent("t", &evtPayload{N: 9}, "my_alias")
	if e.SubType != "my_alias" {
		t.Fatalf("explicit SubType = %q", e.SubType)
	}
	if _, err := Decode(e); err != nil {
		t.Fatalf("Decode explicit subtype: %v", err)
	}
}

func TestDecodeAs(t *testing.T) {
	Register("evtPayload", evtPayload{})
	defer func() { regMu.Lock(); delete(regBy, "evtPayload"); regMu.Unlock() }()

	e := NewEvent("t", evtPayload{N: 4, S: "y"}, "")
	got, err := DecodeAs[evtPayload](e)
	if err != nil || got.N != 4 || got.S != "y" {
		t.Fatalf("DecodeAs = %+v, %v", got, err)
	}
}

func TestDecodeUnregistered(t *testing.T) {
	if _, err := Decode(NewEvent("t", nil, "no_such_type")); err == nil {
		t.Fatal("Decode of unregistered subtype should error")
	}
}

func TestRegisterDuplicateAndIdempotent(t *testing.T) {
	Register("dup_payload", evtPayload{})
	defer func() { regMu.Lock(); delete(regBy, "dup_payload"); regMu.Unlock() }()
	// 同名同型:幂等,不 panic。
	Register("dup_payload", evtPayload{})
	// 同名不同型:panic。
	type other struct{ X int }
	defer func() {
		if recover() == nil {
			t.Fatal("Register same name with different type should panic")
		}
	}()
	Register("dup_payload", other{})
}
