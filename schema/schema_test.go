package schema

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func v(schema, value string) []string {
	return Validate(json.RawMessage(schema), json.RawMessage(value), "value")
}

func TestValidateBooleanSchemas(t *testing.T) {
	for _, input := range []string{`null`, `true`, `{"a":1}`, `"text"`} {
		if got := v(`true`, input); len(got) != 0 {
			t.Fatalf("true schema must accept %s: %v", input, got)
		}
		if got := v(`false`, input); len(got) == 0 {
			t.Fatalf("false schema must reject %s", input)
		}
	}
}

func TestValidateSingleType(t *testing.T) {
	if got := v(`{"type":"integer"}`, `5`); len(got) != 0 {
		t.Fatalf("integer 5: %v", got)
	}
	if got := v(`{"type":"integer"}`, `"5"`); len(got) == 0 {
		t.Fatal(`string "5" should not pass integer`)
	}
}

func TestValidateTypeUnion(t *testing.T) {
	// 一个字段允许多种格式:int 或 string。
	s := `{"type":["integer","string"]}`
	if got := v(s, `5`); len(got) != 0 {
		t.Fatalf("int 5 should pass: %v", got)
	}
	if got := v(s, `"5"`); len(got) != 0 {
		t.Fatalf(`string "5" should pass: %v`, got)
	}
	if got := v(s, `true`); len(got) == 0 {
		t.Fatal("bool should not pass integer|string")
	}
}

func TestValidateAnyOf(t *testing.T) {
	// anyOf:两种形状二选一。
	s := `{"anyOf":[{"type":"integer"},{"type":"string","enum":["auto"]}]}`
	if got := v(s, `7`); len(got) != 0 {
		t.Fatalf("7 should pass: %v", got)
	}
	if got := v(s, `"auto"`); len(got) != 0 {
		t.Fatalf(`"auto" should pass: %v`, got)
	}
	if got := v(s, `"other"`); len(got) == 0 {
		t.Fatal(`"other" should fail anyOf`)
	}
}

func TestValidateNestedObject(t *testing.T) {
	s := `{"type":"object","properties":{"a":{"type":"integer"},"nums":{"type":"array","items":{"type":"integer"}}},"required":["a"]}`
	if got := v(s, `{"a":1,"nums":[1,2,3]}`); len(got) != 0 {
		t.Fatalf("valid object: %v", got)
	}
	if got := v(s, `{"nums":[1,"x"]}`); len(got) == 0 {
		t.Fatal("missing a + bad item should fail")
	}
}

type embeddedBase struct {
	Inner string `json:"inner"` // 无 omitempty → 必填
	Opt   string `json:"opt,omitempty"`
}

type outerWithEmbed struct {
	embeddedBase        // 匿名嵌入,未显式起名 → encoding/json 摊平
	Extra        string `json:"extra"`
}

type outerExplicitName struct {
	Base embeddedBase `json:"Base"` // 显式起名 → 不摊平
}

type outerSkip struct {
	Drop string `json:"-"`
	Keep string `json:"keep"`
}

func keys(fields []wireField) map[string]bool {
	m := map[string]bool{}
	for _, f := range fields {
		m[f.wire] = f.required
	}
	return m
}

func TestStructFieldsEmbeddedFlatten(t *testing.T) {
	// 匿名嵌入应摊平:内部 required 字段提升为顶层必填,而非一个"嵌入类型名"字段。
	got := keys(structFields(reflect.TypeOf(outerWithEmbed{})))
	if !got["inner"] {
		t.Fatalf("flattened embed should surface inner (required): %v", got)
	}
	if got["embeddedBase"] {
		t.Fatalf("embed type name must NOT appear as a field: %v", got)
	}
	if !got["extra"] {
		t.Fatalf("outer field extra missing: %v", got)
	}
}

func TestStructFieldsExplicitNameAndSkip(t *testing.T) {
	got := keys(structFields(reflect.TypeOf(outerExplicitName{})))
	if !got["Base"] {
		t.Fatalf("explicitly-named embed should stay a named field: %v", got)
	}
	skip := structFields(reflect.TypeOf(outerSkip{}))
	for _, f := range skip {
		if f.wire == "Drop" {
			t.Fatalf(`json:"-" field should be skipped: %v`, skip)
		}
	}
	if !keys(skip)["keep"] {
		t.Fatalf("keep field missing: %v", skip)
	}
}

type presetShape struct {
	A string `json:"a"`           // 无 omitempty → 必填
	B int    `json:"b,omitempty"` // omitempty → 非必填
	C string `json:"-"`           // 跳过
}

func TestRequiredFieldsAndValidateMin(t *testing.T) {
	Register("test.preset", "测试预设", presetShape{})
	req := RequiredFields("test.preset")
	if len(req) != 1 || req[0] != "a" {
		t.Fatalf("RequiredFields = %v (want [a], b omitempty + c skip 被排除)", req)
	}
	if err := ValidateMin("test.preset", json.RawMessage(`{"a":"x"}`)); err != nil {
		t.Fatalf("ValidateMin valid: %v", err)
	}
	if err := ValidateMin("test.preset", json.RawMessage(`{"b":1}`)); err == nil {
		t.Fatal("ValidateMin should flag missing required field a")
	}
	if err := ValidateMin("no.such.preset", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ValidateMin of unregistered preset should error")
	}
}

func TestBuildConfigSchema(t *testing.T) {
	s := BuildConfigSchema([]ConfigPair{
		{Key: "k", Schema: json.RawMessage(`{"type":"integer"}`)},
		{Key: "empty", Schema: json.RawMessage("")},       // 空 schema 跳过
		{Key: "bad", Schema: json.RawMessage(`not-json`)}, // 非法 JSON 跳过
	})
	var m map[string]any
	if err := json.Unmarshal(s, &m); err != nil {
		t.Fatalf("built schema not JSON: %v (%s)", err, s)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("properties = %#v", m["properties"])
	}
	if m["type"] != "object" {
		t.Fatalf("type should be object: %v", m["type"])
	}
}

func TestEntriesLookupAndValidateConfig(t *testing.T) {
	Register("test.entries", "目录条目测试", presetShape{})
	ents := Entries()
	found := false
	for _, e := range ents {
		if e.Name == "test.entries" && e.Type == reflect.TypeOf(presetShape{}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("Entries should include test.entries: %+v", ents)
	}
	if _, ok := LookupEntry("test.entries"); !ok {
		t.Fatal("LookupEntry should find test.entries")
	}
	if _, ok := LookupEntry("no.such"); ok {
		t.Fatal("LookupEntry of unknown should fail")
	}
	// ValidateConfig 是 Validate 以 root="config" 命名的便捷面。
	if got := ValidateConfig(json.RawMessage(`{"type":"integer"}`), json.RawMessage(`5`)); len(got) != 0 {
		t.Fatalf("ValidateConfig integer 5: %v", got)
	}
	if got := ValidateConfig(json.RawMessage(`{"type":"integer"}`), json.RawMessage(`"5"`)); len(got) == 0 {
		t.Fatal("ValidateConfig should reject string \"5\" for integer")
	}
}

func TestValidateDocumentAllowsDocumentLocalReferences(t *testing.T) {
	document := json.RawMessage(`{"$defs":{"count":{"type":"integer","minimum":1}},"$ref":"#/$defs/count"}`)
	if err := ValidateDocument(document); err != nil {
		t.Fatalf("document-local reference: %v", err)
	}
	if got := Validate(document, json.RawMessage(`2`), "value"); len(got) != 0 {
		t.Fatalf("valid local-ref value: %v", got)
	}
	if got := Validate(document, json.RawMessage(`0`), "value"); len(got) == 0 {
		t.Fatal("local-ref minimum must still be enforced")
	}
}

func TestSchemaRejectsExternalFileReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(path, []byte(`{"type":"integer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reference := (&url.URL{Scheme: "file", Path: path}).String()
	document, err := json.Marshal(map[string]any{"$ref": reference})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDocument(document); err == nil {
		t.Fatal("ValidateDocument loaded an external file reference")
	}
	if got := Validate(document, json.RawMessage(`1`), "value"); len(got) == 0 {
		t.Fatal("Validate loaded an external file reference")
	}
}

func TestSchemaNeverFetchesRemoteReference(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = fmt.Fprint(w, `{"type":"integer"}`)
	}))
	defer server.Close()
	document, err := json.Marshal(map[string]any{"$ref": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDocument(document); err == nil {
		t.Fatal("ValidateDocument accepted a remote reference")
	}
	if got := Validate(document, json.RawMessage(`1`), "value"); len(got) == 0 {
		t.Fatal("Validate accepted a remote reference")
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("remote reference fetched %d times", got)
	}
}
