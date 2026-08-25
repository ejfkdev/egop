// Package schema 是预设结构目录的表机制与配置/最小形状校验（内容无关）。
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// Entry 目录条目。
type Entry struct {
	Name string
	Doc  string
	Type reflect.Type
}

var (
	mu      sync.Mutex
	entries = map[string]Entry{}
	order   []string
)

// Register 登记预设结构（幂等;同名不同型 panic）。
func Register(name, doc string, proto any) {
	t := reflect.TypeOf(proto)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic("schema: registered type must be a struct: " + name)
	}
	mu.Lock()
	defer mu.Unlock()
	if old, ok := entries[name]; ok {
		if old.Type == t {
			return
		}
		panic("schema: preset name " + name + " collides")
	}
	order = append(order, name)
	entries[name] = Entry{Name: name, Doc: doc, Type: t}
}

// Entries 全量目录（登记序）。
func Entries() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, len(order))
	for _, n := range order {
		out = append(out, entries[n])
	}
	return out
}

// LookupEntry 按名取条目。
func LookupEntry(name string) (Entry, bool) {
	for _, e := range Entries() {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Validate 按 JSON Schema 子集校验 value,问题路径化（空=通过）。root 是顶层
// 路径名（如 "input"/"output"/"config"）。schema 为空/null 时不校验。
// type 可为单个类型串或类型数组（任意其一即通过）;anyOf 为候选子 schema 数组
// （任一通过即通过）——均用于"一个字段允许多种格式"。
func Validate(schema json.RawMessage, value json.RawMessage, root string) []string {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return []string{fmt.Sprintf("schema: %v", err)}
	}
	return validateNode(s, value, root)
}

// ValidateConfig 校验配置——Validate 以 root 命名 "config" 的便捷面。
func ValidateConfig(schema json.RawMessage, cfg json.RawMessage) []string {
	return Validate(schema, cfg, "config")
}

// ConfigPair 配置字段声明的最小形状。
type ConfigPair struct {
	Key    string
	Schema json.RawMessage
}

// BuildConfigSchema 复合字段声明为 object schema。
func BuildConfigSchema(fields []ConfigPair) json.RawMessage {
	props := map[string]any{}
	for _, f := range fields {
		if len(f.Schema) == 0 {
			continue
		}
		var node map[string]any
		if err := json.Unmarshal(f.Schema, &node); err == nil {
			props[f.Key] = node
		}
	}
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
	return b
}

// RequiredFields 按登记类型反射必填字段（最小形状）。
func RequiredFields(name string) []string {
	e, ok := LookupEntry(name)
	if !ok {
		return nil
	}
	var req []string
	for _, f := range structFields(e.Type) {
		if f.required {
			req = append(req, f.wire)
		}
	}
	return req
}

// ValidateMin 最小形状校验（顶层必填覆盖;多余字段永不报错）。
func ValidateMin(name string, payload json.RawMessage) error {
	e, ok := LookupEntry(name)
	if !ok {
		return fmt.Errorf("schema: preset %q not registered", name)
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(payload, &v); err != nil {
		return fmt.Errorf("schema: %s: bad payload: %w", name, err)
	}
	var missing []string
	for _, f := range structFields(e.Type) {
		if _, present := v[f.wire]; f.required && !present {
			missing = append(missing, f.wire)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("schema: %s: missing required fields: %v", name, missing)
	}
	return nil
}

type wireField struct {
	name     string
	wire     string
	required bool
}

func structFields(t reflect.Type) []wireField {
	var out []wireField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		// 与 encoding/json 一致的可见性规则:
		//  - 匿名的非结构体未导出字段:忽略;
		//  - 匿名的结构体字段(即使类型未导出):继续处理,内部可能含导出子字段;
		//  - 非匿名的未导出字段:忽略。
		if sf.Anonymous {
			ft := sf.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if sf.PkgPath != "" && ft.Kind() != reflect.Struct {
				continue
			}
		} else if sf.PkgPath != "" {
			continue
		}

		tag := sf.Tag.Get("json")
		wire := sf.Name
		omit := false
		explicitName := false
		if tag != "" {
			parts := splitTag(tag)
			if parts[0] == "-" { // json:"-":不参与 JSON,跳过
				continue
			}
			if parts[0] != "" {
				wire = parts[0]
				explicitName = true
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omit = true
				}
			}
		}
		// encoding/json 提升语义:匿名结构体字段且未显式起名 → 摊平其内部字段
		// (内部 required 一并提升,而非把整个嵌入体当成一个顶层必填字段)。
		if sf.Anonymous {
			ft := sf.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !explicitName {
				out = append(out, structFields(ft)...)
				continue
			}
		}
		out = append(out, wireField{name: sf.Name, wire: wire, required: !omit && sf.Type.Kind() != reflect.Ptr})
	}
	return out
}

func splitTag(tag string) []string {
	var out []string
	cur := ""
	for _, r := range tag {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func validateNode(s map[string]any, v json.RawMessage, path string) []string {
	// anyOf:任一子 schema 通过即通过（覆盖"该字段是 a 或 b 两种形状"的诉求）。
	if subs, ok := s["anyOf"].([]any); ok {
		for _, raw := range subs {
			sub, _ := raw.(map[string]any)
			if sub != nil && len(validateNode(sub, v, path)) == 0 {
				return nil
			}
		}
		return []string{fmt.Sprintf("%s: matches none of anyOf", path)}
	}
	types := typeNames(s["type"])
	if len(types) == 0 {
		return nil
	}
	var first []string
	for _, t := range types {
		if iss := validateType(v, path, t, s); len(iss) == 0 {
			return nil
		} else if first == nil {
			first = iss
		}
	}
	if len(types) == 1 {
		return first
	}
	return []string{fmt.Sprintf("%s: expected one of [%s]", path, strings.Join(types, ", "))}
}

// typeNames 归一化 "type":既可是单类型串,也可是类型数组。
func typeNames(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func validateType(v json.RawMessage, path, typ string, s map[string]any) []string {
	switch typ {
	case "object":
		return validateObject(v, path, s)
	case "array":
		return validateArray(v, path, s)
	case "string":
		return validateString(v, path, s)
	case "integer":
		return validateInteger(v, path)
	case "number":
		return validateNumber(v, path)
	case "boolean":
		return validateBoolean(v, path)
	default:
		return []string{fmt.Sprintf("%s: unsupported type %q", path, typ)}
	}
}

func validateObject(v json.RawMessage, path string, s map[string]any) []string {
	if len(v) == 0 {
		return nil
	}
	var out []string
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return append(out, fmt.Sprintf("%s: expected object", path))
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for key, raw := range props {
			sub, _ := raw.(map[string]any)
			if sub == nil {
				continue
			}
			if rawv, present := obj[key]; present {
				out = append(out, validateNode(sub, rawv, join(path, key))...)
			}
		}
	}
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				if _, present := obj[name]; !present {
					out = append(out, fmt.Sprintf("%s.%s: required", path, name))
				}
			}
		}
	}
	return out
}

func validateArray(v json.RawMessage, path string, s map[string]any) []string {
	var out []string
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return append(out, fmt.Sprintf("%s: expected array", path))
	}
	if items, ok := s["items"].(map[string]any); ok {
		for i, item := range arr {
			out = append(out, validateNode(items, item, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return out
}

func validateString(v json.RawMessage, path string, s map[string]any) []string {
	var out []string
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return append(out, fmt.Sprintf("%s: expected string", path))
	} else if en, ok := s["enum"].([]any); ok && !enumContains(en, str) {
		out = append(out, fmt.Sprintf("%s: value %q not in enum", path, str))
	}
	return out
}

func validateInteger(v json.RawMessage, path string) []string {
	var n float64
	if err := json.Unmarshal(v, &n); err != nil || n != float64(int64(n)) {
		return []string{fmt.Sprintf("%s: expected integer", path)}
	}
	return nil
}

func validateNumber(v json.RawMessage, path string) []string {
	var n float64
	if err := json.Unmarshal(v, &n); err != nil {
		return []string{fmt.Sprintf("%s: expected number", path)}
	}
	return nil
}

func validateBoolean(v json.RawMessage, path string) []string {
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return []string{fmt.Sprintf("%s: expected boolean", path)}
	}
	return nil
}

func join(p, s string) string {
	if p == "" {
		return s
	}
	return p + "." + s
}

func enumContains(list []any, want string) bool {
	for _, e := range list {
		if s, ok := e.(string); ok && s == want {
			return true
		}
	}
	return false
}
