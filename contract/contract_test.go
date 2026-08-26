package contract

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestResultEnvelopeRichFields(t *testing.T) {
	e := ResultEnvelope{
		OK:     true,
		Result: json.RawMessage(`42`),
		Type:   "integer",
		At:     1234567890,
		Meta:   json.RawMessage(`{"correlation":"abc"}`),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back ResultEnvelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.OK ||
		string(back.Result) != "42" ||
		back.Type != "integer" ||
		back.At != 1234567890 ||
		string(back.Meta) != `{"correlation":"abc"}` {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	// 未设置的可选字段不落 JSON(omitempty)。
	zb, err := json.Marshal(ResultEnvelope{})
	if err != nil {
		t.Fatal(err)
	}
	if string(zb) != `{"ok":false}` {
		t.Fatalf("zero envelope json = %s", zb)
	}
}

func TestMetaDescriptiveFields(t *testing.T) {
	// 描述性元数据(主页/许可/作者/关键词)与配置敏感标记在 JSON 上往返不丢。
	m := Meta{
		ID: "p.x", Name: "X", Version: "1.0.0",
		Homepage: "https://example.com/p",
		License:  "MIT",
		Authors:  []string{"alice", "bob"},
		Tags:     []string{"auth", "io"},
		Provides: Provides{Config: []ConfigFieldSpec{{Key: "api_key", Writable: true, Secret: true}}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Meta
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Homepage != "https://example.com/p" || back.License != "MIT" ||
		len(back.Authors) != 2 || back.Authors[1] != "bob" ||
		len(back.Tags) != 2 || back.Tags[0] != "auth" {
		t.Fatalf("meta descriptive fields lost: %+v", back)
	}
	if len(back.Provides.Config) != 1 || !back.Provides.Config[0].Secret || !back.Provides.Config[0].Writable {
		t.Fatalf("config secret/writable lost: %+v", back.Provides.Config)
	}
}

func TestOriginRoundTrip(t *testing.T) {
	o := Origin{ID: "p.x", Version: "1.0.0", Kind: OriginEvent, Point: "topic.a", At: 1234567890}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var back Origin
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != "p.x" || back.Version != "1.0.0" || back.Kind != OriginEvent || back.Point != "topic.a" || back.At != 1234567890 {
		t.Fatalf("origin round-trip lost fields: %+v", back)
	}
}

func TestOriginContext(t *testing.T) {
	ctx := context.Background()
	if OriginFrom(ctx) != nil {
		t.Fatal("OriginFrom without injection should be nil")
	}
	ctx = WithOrigin(ctx, &Origin{ID: "caller", Kind: OriginCall, Point: "do"})
	got := OriginFrom(ctx)
	if got == nil || got.ID != "caller" || got.Kind != OriginCall || got.Point != "do" {
		t.Fatalf("OriginFrom = %+v", got)
	}
}

func TestHookResultOf(t *testing.T) {
	// 归一规则:完整形态原样、裸数据进 Data、nil 空结果。
	hr := HookResultOf(&HookResult{Block: true, Reason: "r"})
	if !hr.Block || hr.Reason != "r" {
		t.Fatalf("full form passthrough = %+v", hr)
	}
	if hr.Data != nil {
		t.Fatalf("full form Data should be untouched: %s", hr.Data)
	}

	raw := HookResultOf(json.RawMessage(`{"k":1}`))
	if string(raw.Data) != `{"k":1}` {
		t.Fatalf("RawMessage data = %s", raw.Data)
	}

	bytes := HookResultOf([]byte(`[1,2]`))
	if string(bytes.Data) != `[1,2]` {
		t.Fatalf("[]byte data = %s", bytes.Data)
	}

	val := HookResultOf(struct{ N int }{N: 7})
	if string(val.Data) != `{"N":7}` {
		t.Fatalf("struct marshal data = %s", val.Data)
	}

	if got := HookResultOf(nil); got.Block || got.Reason != "" || got.Data != nil || got.Origin != nil || got.Seq != 0 {
		t.Fatalf("nil should produce zero HookResult: %+v", got)
	}
}

func TestNetWireContract(t *testing.T) {
	// Request/Response 是跨边界(远程插件 / WASM ABI)JSON 信封的载荷:
	// method/url/headers/trailers 在 JSON 上往返不丢,Body(io.Reader)不参与序列化。
	req := Request{
		Method:  "POST",
		URL:     "https://api.example.com/v1/chat",
		Headers: map[string]string{"content-type": "application/json", "authorization": "Bearer t"},
		Body:    strings.NewReader("raw-body"),
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "raw-body") {
		t.Fatalf("request Body must not serialize (json:\"-\"): %s", b)
	}
	var rback Request
	if err := json.Unmarshal(b, &rback); err != nil {
		t.Fatal(err)
	}
	if rback.Method != "POST" || rback.URL != "https://api.example.com/v1/chat" ||
		rback.Headers["authorization"] != "Bearer t" || rback.Body != nil {
		t.Fatalf("request round-trip lost fields: %+v", rback)
	}

	resp := Response{
		Status:   200,
		Headers:  map[string]string{"content-type": "application/grpc-web"},
		Trailers: map[string]string{"grpc-status": "0", "grpc-message": "ok"},
		Body:     strings.NewReader("raw-resp"),
	}
	rb, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var pback Response
	if err := json.Unmarshal(rb, &pback); err != nil {
		t.Fatal(err)
	}
	if pback.Status != 200 || pback.Headers["content-type"] != "application/grpc-web" ||
		pback.Trailers["grpc-status"] != "0" || pback.Trailers["grpc-message"] != "ok" {
		t.Fatalf("response trailers/headers lost: %+v", pback)
	}
	// Trailers 为空时不落 JSON(omitempty)。
	zb, err := json.Marshal(Response{Status: 204})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zb), "trailers") {
		t.Fatalf("zero trailers must be omitted: %s", zb)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"chat.message", "chat.message", true},
		{"chat.message", "chat.other", false},
		{"*", "anything", true},
		{"*", "", true},
		{"chat.*", "chat.message", true},
		{"chat.*", "chat.", true},
		{"chat.*", "room.message", false},
		{"*.done", "task.done", true},
		{"*.done", "task.pending", false},
		{"a.*.c", "a.b.c", true},
		{"a.*.c", "a.b.b.c", true},
		{"a.*.c", "a.c", false},
		{"*mid*", "xxmidyy", true},
		{"pre*post", "pre-mid-post", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestEventFilterMatch(t *testing.T) {
	ev := Event{
		Type:    "chat.message",
		SubType: "text",
		Source:  &Origin{ID: "demo.producer", Version: "2.1.0", Kind: OriginEvent},
		Labels:  map[string]string{"sev": "high", "room": "lobby"},
	}

	// 空 filter 命中一切。
	if !(EventFilter{}).Match(ev) {
		t.Fatal("zero filter should match everything")
	}
	// 主题通配 + 多字段组合。
	if !(EventFilter{Type: "chat.*", SourceID: "demo.producer", SourceVersion: "2.1.0", Labels: map[string]string{"sev": "high"}}).Match(ev) {
		t.Fatal("wildcard + source + version + label should match")
	}
	// 不满足任一条件即不命中。
	for _, f := range []EventFilter{
		{Type: "room.*"},
		{SubType: "image"},
		{SourceID: "other"},
		{SourceVersion: "1.0.0"},
		{SourceKind: OriginHook},
		{Labels: map[string]string{"sev": "low"}},
		{Labels: map[string]string{"missing": "x"}},
	} {
		if f.Match(ev) {
			t.Fatalf("filter %+v should NOT match %+v", f, ev)
		}
	}
	// 标签子集语义:要求的部分键值都在才命中。
	if !(EventFilter{Labels: map[string]string{"sev": "high"}}).Match(ev) {
		t.Fatal("label subset should match")
	}
	// Source 为空时不匹配带来源过滤的条件。
	if (EventFilter{SourceID: "x"}).Match(Event{Type: "t"}) {
		t.Fatal("nil source should not match SourceID filter")
	}
}

func TestHasCapabilityAndNamespaceIDs(t *testing.T) {
	m := Meta{Provides: Provides{Capabilities: []string{CapCallPlugins}}}
	if !HasCapability(m, CapCallPlugins) || HasCapability(m, CapNet) {
		t.Fatalf("HasCapability call=%v net=%v", HasCapability(m, CapCallPlugins), HasCapability(m, CapNet))
	}
	// 命名空间化前缀(动态点位/事件主题的稳定键)。
	if PointID("p", "x") != DynamicPrefix+"p.x" || EventID("p", "evt") != DynamicPrefix+"p.evt" {
		t.Fatalf("namespace ids: %q %q", PointID("p", "x"), EventID("p", "evt"))
	}
}

func TestConfigFieldSpecRoundTrip(t *testing.T) {
	// 配置字段声明(含 Default/Secret/Read-Write 标志)在 JSON 上往返不丢。
	m := Meta{Provides: Provides{Config: []ConfigFieldSpec{{
		Key: "api_key", Description: "key", Schema: json.RawMessage(`{"type":"string"}`),
		Default: json.RawMessage(`"https://default"`), Writable: true, Secret: true,
	}}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Meta
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	f := back.Provides.Config[0]
	if !f.Writable || !f.Secret || string(f.Default) != `"https://default"` ||
		string(f.Schema) != `{"type":"string"}` {
		t.Fatalf("config field lost: %+v", f)
	}
}

func TestMetaExtensionsRoundTrip(t *testing.T) {
	// 自由扩展键值(Extentions):任意 key → 任意 JSON 值,egop 不解释、原样透传。
	m := Meta{
		ID: "p.x", Name: "X", Version: "1",
		Extensions: map[string]json.RawMessage{
			"custom.capability": json.RawMessage(`"my.word"`),
			"ui.order":          json.RawMessage(`10`),
			"badge":             json.RawMessage(`{"logo":"https://x/logo.png","color":"#fff"}`),
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Meta
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Extensions) != 3 ||
		string(back.Extensions["custom.capability"]) != `"my.word"` ||
		string(back.Extensions["ui.order"]) != `10` ||
		string(back.Extensions["badge"]) != `{"logo":"https://x/logo.png","color":"#fff"}` {
		t.Fatalf("extensions lost: %+v", back.Extensions)
	}
	// 未设置时不落 JSON(omitempty)。
	zb, _ := json.Marshal(Meta{ID: "x"})
	if strings.Contains(string(zb), "extensions") {
		t.Fatalf("zero extensions must be omitted: %s", zb)
	}
}
