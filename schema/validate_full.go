package schema

import (
	"bytes"
	"encoding/json"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// localOnlyLoader 拒绝编译器尚未内嵌的一切资源。根文档经 AddResource 注入,
// 标准元 schema 由依赖库的内嵌数据提供;此外绝不读文件或访问网络。
type localOnlyLoader struct{}

func (localOnlyLoader) Load(location string) (any, error) {
	return nil, fmt.Errorf("external schema resource %q is not allowed", location)
}

// ValidateDocument 只编译校验 JSON Schema 文档本身,不校验实例。文档内
// #/$defs 等本地引用可正常解析;任何需要外部资源的引用都在编译期拒绝。
func ValidateDocument(document json.RawMessage) error {
	if _, err := compileDocument(document); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}

func compileDocument(document json.RawMessage) (*jsonschema.Schema, error) {
	var decoded any
	if err := json.NewDecoder(bytes.NewReader(document)).Decode(&decoded); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(localOnlyLoader{})
	const location = "https://egop.local/schema"
	if err := compiler.AddResource(location, decoded); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

// validateStrict 按 draft-2020-12 校验一个 JSON 值。
// 数字使用 json.Number,避免大整数和整数/数值类型判定失真。
func validateStrict(schema, value json.RawMessage, root string) []string {
	compiled, err := compileDocument(schema)
	if err != nil {
		return []string{fmt.Sprintf("schema: %v", err)}
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var actual any
	if err := decoder.Decode(&actual); err != nil {
		return []string{fmt.Sprintf("%s: %v", root, err)}
	}
	if err := compiled.Validate(actual); err != nil {
		return []string{fmt.Sprintf("%s: %v", root, err)}
	}
	return nil
}
