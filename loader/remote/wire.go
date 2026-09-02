// 线上约定:信封/op 语法(HostCall 的参数与结果形状)——与 WASM 插件 ABI 同构。
// 能力词守卫与处理器一律经装配注入(host.Options),本包零业务类型。
package remote

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/schema"
)

// ResultEnvelope 是跨世界调用的统一结果信封(真源 contract.ResultEnvelope)。
type ResultEnvelope = contract.ResultEnvelope

// init 把跨世界统一结果信封登记进 core 预设结构目录(wasm/远程通道同款契约)。
func init() {
	schema.Register("ResultEnvelope", "跨世界调用的统一结果信封:{ok,result,result_b64,error}", ResultEnvelope{})
}

// envelope 是跨边界统一结果信封的包内短名。
type envelope = ResultEnvelope

func okEnvelope(raw json.RawMessage) []byte { return mustMarshal(envelope{OK: true, Result: raw}) }
func b64Envelope(b []byte) []byte {
	return mustMarshal(envelope{OK: true, ResultB64: base64.StdEncoding.EncodeToString(b)})
}
func errEnvelope(err error) []byte { return mustMarshal(envelope{Error: err.Error()}) }

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"ok":false,"error":"marshal envelope"}`)
	}
	return b
}

// HostCall op 词汇(与 wasm 宿主注入短名一致)。
const (
	OpCall         = "call"
	OpGetSetting   = "get_setting"
	OpPersistRead  = "persist_read"
	OpPersistWrite = "persist_write"
	OpPersistList  = "persist_list"
	OpKVGet        = "kv_get"
	OpKVPut        = "kv_put"
	OpKVDelete     = "kv_delete"
	OpKVKeys       = "kv_keys"
	OpExec         = "exec"
	OpOnHook       = "on_hook"
	OpPublishEvent = "publish_event"
	OpPlugins      = "plugins"
	OpGetPlugin    = "get_plugin"
	OpGetConfig    = "get_config"
	OpSetConfig    = "set_config"
	OpFSRead       = "fs_read"
	OpFSWrite      = "fs_write"
	// OpNetRequest / OpNetBodyRead / OpNetBodyClose 出站网络三 op(与 wasm ABI
	// net_request/net_body_read/net_body_close 同词汇同形状;响应 body 流式读,
	// chunk 走 base64)。
	OpNetRequest   = "net_request"
	OpNetBodyRead  = "net_body_read"
	OpNetBodyClose = "net_body_close"
)

// HostCall 各 op 的参数 JSON 形状。
type (
	callArgs struct {
		PluginID string          `json:"plugin_id"`
		Fname    string          `json:"fname"`
		Input    json.RawMessage `json:"input"`
	}
	keyArgs struct {
		Key string `json:"key"`
	}
	readArgs struct {
		Name string `json:"name"`
	}
	writeArgs struct {
		Name    string `json:"name"`
		DataB64 string `json:"data_b64"`
	}
	putArgs struct {
		Key      string `json:"key"`
		ValueB64 string `json:"value_b64"`
	}
	execArgs struct {
		Cmd string `json:"cmd"`
	}
	handleArgs struct {
		Handle string `json:"handle"`
	}
	publishArgs struct {
		Topic   string            `json:"topic"`
		SubType string            `json:"sub_type,omitempty"`
		Labels  map[string]string `json:"labels,omitempty"`
		Payload json.RawMessage   `json:"payload"`
	}
)

// netChunkSize 是 net_body_read 单次读取上限(与 wasm ABI 同款 32KiB;chunk 走
// base64,单帧远小于 maxFrameSize)。
const netChunkSize = 32 * 1024

// decodeEnvelope 解结果信封;信封错误 → error。
func decodeEnvelope(payload json.RawMessage) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("bad result envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", env.Error)
	}
	if env.ResultB64 != "" {
		data, err := base64.StdEncoding.DecodeString(env.ResultB64)
		if err != nil {
			return nil, fmt.Errorf("bad result_b64: %w", err)
		}
		return data, nil
	}
	return env.Result, nil
}

// b64bytes 是共享解码助手。
func b64bytes(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
