package host

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactJSON 敏感键脱敏:嵌套/数组/大小写/空值三态/非 JSON 原样。
func TestRedactJSON(t *testing.T) {
	in := json.RawMessage(`{
		"kind": "anthropic",
		"base_url": "https://api.example.com",
		"token": "sk-SECRET-VALUE",
		"API_KEY": "AK-SECRET",
		"empty_token": "",
		"nested": {"client_secret": "s3cret", "name": "ok"},
		"list": [{"password": "p"}, {"safe": 1}]
	}`)
	out := string(redactJSON(in))
	for _, leak := range []string{"sk-SECRET-VALUE", "AK-SECRET", "s3cret", `"p"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("密钥泄漏进观察事件: %s\n%s", leak, out)
		}
	}
	for _, keep := range []string{"anthropic", "api.example.com", `"ok"`, `"safe":1`, redactedMark} {
		if !strings.Contains(out, keep) {
			t.Fatalf("非敏感内容丢失 %q: %s", keep, out)
		}
	}
	if !strings.Contains(out, `"empty_token":""`) {
		t.Fatalf("空值应保留(三态语义): %s", out)
	}
	// 非 JSON 对象:原样返回
	if got := string(redactJSON(json.RawMessage(`not json`))); got != "not json" {
		t.Fatalf("非 JSON 应原样: %s", got)
	}
}

// TestRedactJSONWithDeclared 声明优先:顶层命中 ConfigFieldSpec.Secret 的键
// 无条件脱敏(键名不敏感也遮);未声明键仍走键名启发(含嵌套层)。
func TestRedactJSONWithDeclared(t *testing.T) {
	in := json.RawMessage(`{
		"api_url": "https://svc/secret-path",
		"note": "hello",
		"nested": {"client_secret": "s3cret"}
	}`)
	out := string(redactJSONWith(in, map[string]bool{"api_url": true}))
	if !strings.Contains(out, redactedMark) {
		t.Fatalf("declared secret must be redacted: %s", out)
	}
	if strings.Contains(out, "secret-path") {
		t.Fatalf("declared secret leaked: %s", out)
	}
	if !strings.Contains(out, `"note":"hello"`) {
		t.Fatalf("non-secret lost: %s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatalf("heuristic fallback must still apply to nested keys: %s", out)
	}
}
