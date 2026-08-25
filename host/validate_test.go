package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ejfkdev/egop/contract"
)

// schemaFunc 声明了 Input/Output schema 的函数插件(结算 a+b)。
type schemaFunc struct{}

func (schemaFunc) Meta() contract.Meta {
	return contract.Meta{
		ID: "vf.add", Name: "Validated", Version: "1",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{{
				Name:   "add",
				Input:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
				Output: json.RawMessage(`{"type":"integer"}`),
			}},
		},
	}
}

func (schemaFunc) CallFunc(_ context.Context, _ string, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	_ = json.Unmarshal(input, &in)
	return json.Marshal(in.A + in.B)
}

func TestCallValidatesInputSchema(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(schemaFunc{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(context.Background(), "vf.add", "add", json.RawMessage(`{"a":1,"b":2}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	// 缺 b 且 a 是 string → 应拒绝,错误路径带 input。
	_, err := h.Call(context.Background(), "vf.add", "add", json.RawMessage(`{"a":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "input") {
		t.Fatalf("bad input err = %v", err)
	}
}

// badOut 声明 Output 为 string 却返回 JSON 数字,用于校验返回。
type badOut struct{}

func (badOut) Meta() contract.Meta {
	return contract.Meta{
		ID: "vf.out", Name: "BadOut", Version: "1",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{{Name: "n", Output: json.RawMessage(`{"type":"string"}`)}},
		},
	}
}

func (badOut) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`42`), nil
}

func TestCallValidatesOutputSchema(t *testing.T) {
	h := New[any](Options[any]{})
	if err := h.Register(badOut{}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Call(context.Background(), "vf.out", "n", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("bad output should be rejected: %v", err)
	}
}

// ignoreInput 忽略入参、恒返回 "ok",用于对照开关校验的区别。
type ignoreInput struct{}

func (ignoreInput) Meta() contract.Meta {
	return contract.Meta{
		ID: "vf.ignore", Name: "Ignore", Version: "1",
		Provides: contract.Provides{
			Functions: []contract.FuncSpec{{Name: "f", Input: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}`)}},
		},
	}
}

func (ignoreInput) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

func TestCallValidationDisabled(t *testing.T) {
	// 关闭校验:缺 a 的入参直通插件。
	h := New[any](Options[any]{DisableFuncValidation: true})
	if err := h.Register(ignoreInput{}); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Call(context.Background(), "vf.ignore", "f", json.RawMessage(`{}`)); err != nil || string(out) != `"ok"` {
		t.Fatalf("disabled: out=%s err=%v", out, err)
	}

	// 对照:开启校验(默认)时同样入参应被拒。
	h2 := New[any](Options[any]{})
	if err := h2.Register(ignoreInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.Call(context.Background(), "vf.ignore", "f", json.RawMessage(`{}`)); err == nil {
		t.Fatal("enabled validation should reject missing required field")
	}
}
