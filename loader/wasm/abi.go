// wasm 插件 ABI 词汇与宿主注入函数表。
//
// 跨边界约定（唯一通道,与 exchange 同哲学——全程 JSON、无第二种编码）:
//   - 全部字符串 = guest linear memory 内的 UTF-8 字节,(ptr,len) 成对传参;
//   - 宿主注入函数(module "egop")返回 i64 打包 (len<<32)|ptr,指向经 guest 导出
//     egop_host_alloc 分配、位于 guest 内存的结果信封
//     {"ok":bool,"result":any,"result_b64":string,"error":string}:ok=false 时
//     error 为人类可读消息;文件/KV 等二进制值走 result_b64(base64)。
//   - guest 导出函数除 egop_meta(裸 JSON)与 egop_on_event(事件推送回调,无返回)
//     外,一律返回同一信封格式的 (ptr,len)。
package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ABI 名称常量:guest 导出(egop_*)与宿主注入(egop 模块内短名)。
const (
	// HostModuleName 是宿主注入函数所在的模块名(guest import "egop")。
	HostModuleName = "egop"
	// WASIModule 是 guest 声明 WASI 时的模块名(允许的另一个 import)。
	WASIModule = "wasi_snapshot_preview1"

	// ExportHostAlloc guest 必选:宿主往 guest 内存写参数字节前的分配函数。
	ExportHostAlloc = "egop_host_alloc"
	// ExportMeta 无自定义清单段时的清单来源:返回裸 contract.Manifest JSON。
	ExportMeta = "egop_meta"
	// ExportInit 生命周期钩子:注册完成(SetSurface)后宿主调一次。
	ExportInit = "egop_init"
	// ExportCall 声明函数调用(Meta.Provides.Functions 的来源)。
	ExportCall = "egop_call"
	// ExportTool 工具调用(Manifest.Tools 的来源;tctx = 线上 JSON)。
	ExportTool = "egop_tool"
	// ExportApplyConfig 配置下发(SetConfig)。
	ExportApplyConfig = "egop_apply_config"
	// ExportGetConfig 读回当前生效配置(ConfigProvider.Config;返回裸 JSON (ptr,len),
	// 未导出则宿主回退 applied 缓存)。
	ExportGetConfig = "egop_get_config"
	// ExportOnEvent 事件推送回调(宿主调 guest;无返回、尽力而为)。
	ExportOnEvent = "egop_on_event"
	// ExportOnHook hook 回调触发(宿主调 guest;返回 HookResult 信封)。
	ExportOnHook = "egop_on_hook"
	// ExportShutdown 卸载钩子(Close 时尽力而为)。
	ExportShutdown = "egop_shutdown"

	// ImportCall Surface.Call:跨插件函数调用。
	ImportCall = "call"
	// ImportPlugins / ImportGetPlugin 插件目录面(需声明 plugin.meta 能力)。
	ImportPlugins   = "plugins"
	ImportGetPlugin = "get_plugin"
	// ImportGetConfig / ImportSetConfig 跨插件配置面(需 config.read / config.write)。
	ImportGetConfig = "get_config"
	ImportSetConfig = "set_config"
	// ImportGetSetting Surface.GetSetting:配置面查询。
	ImportGetSetting = "get_setting"
	// ImportPersistRead Surface.Persist:读文件(结果信封 result_b64)。
	ImportPersistRead = "persist_read"
	// ImportPersistWrite Surface.Persist:写文件。
	ImportPersistWrite = "persist_write"
	// ImportPersistList Surface.Persist:列文件。
	ImportPersistList = "persist_list"
	// ImportKVGet Surface.KV:取值。
	ImportKVGet = "kv_get"
	// ImportKVPut Surface.KV:写值。
	ImportKVPut = "kv_put"
	// ImportKVDelete Surface.KV:删键。
	ImportKVDelete = "kv_delete"
	// ImportKVKeys Surface.KV:键清单。
	ImportKVKeys = "kv_keys"
	// ImportExec Surface.Exec:执行命令。
	ImportExec = "exec"
	// ImportOp Surface.Op 扩展能力:第 1 参 = op 名、第 2 参 = 入参 JSON(内容无关;
	// 业务 op 名与处理器由装配层注入)。
	ImportOp = "op"
	// ImportPublishEvent Surface.PublishEvent:发布观察事件。
	ImportPublishEvent = "publish_event"
	// ImportSubscribeEvent Surface.SubscribeEvent:订阅主题(事件经 egop_on_event 推回)。
	ImportSubscribeEvent = "subscribe_event"
	// ImportOnHook Surface.OnHook:注册 hook 回调(触发经 egop_on_hook 回 guest)。
	ImportOnHook = "on_hook"
	// ImportReadAsset 读 .egop.zip 内 assets/ 静态资源。
	ImportReadAsset = "read_asset"
	// ImportLog 日志直出:经 Options.LogFn 接入宿主日志面(nil = 静默)。
	ImportLog = "log"

	// ManifestSection 是 .egop.wasm 内置清单的自定义段名。
	ManifestSection = "egop.manifest"

	// SuffixWasm / SuffixZip 是插件文件命名后缀约定(*.egop.wasm / *.egop.zip)。
	SuffixWasm = ".egop.wasm"
	SuffixZip  = ".egop.zip"
)

// hostImportSet 是宿主注入函数的完整名字表(import 白名单校验用)。
var hostImportSet = func() map[string]bool {
	m := map[string]bool{}
	for _, op := range hostOps {
		m[op.name] = true
	}
	return m
}()

// envelope 是跨边界统一结果信封——直接别名 contract.ResultEnvelope(唯一真源),
// 与远程通道(loader/remote)同款,避免两处各自定义导致线上格式漂移。
type envelope = contract.ResultEnvelope

// pack 把 (ptr,len) 打包为 i64;(len<<32)|ptr,与 guest 侧约定一致。
func pack(ptr, length uint32) uint64 { return uint64(length)<<32 | uint64(ptr) }

// unpack 拆 i64 为 (ptr,len)。
func unpack(v uint64) (ptr, length uint32) {
	return uint32(v), uint32(v >> 32)
}

// readGuestString 从 guest 内存读 (ptr,len) 字节串(长度 0 返回空且不越界检查)。
func readGuestString(mem api.Memory, ptr, length uint32) (string, error) {
	if length == 0 {
		return "", nil
	}
	b, ok := mem.Read(ptr, length)
	if !ok {
		return "", fmt.Errorf("wasm plugin: guest pointer out of range (%d..%d)", ptr, uint64(ptr)+uint64(length))
	}
	return string(b), nil
}

// allocWriteGuest 经 guest 导出的 egop_host_alloc 在 guest 内存落 data,返回指针。
func allocWriteGuest(ctx context.Context, m api.Module, data []byte) (uint32, error) {
	fn := m.ExportedFunction(ExportHostAlloc)
	if fn == nil {
		return 0, fmt.Errorf("wasm plugin: export %q missing", ExportHostAlloc)
	}
	results, err := fn.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("wasm plugin: %s: %w", ExportHostAlloc, err)
	}
	if len(results) != 1 {
		return 0, fmt.Errorf("wasm plugin: %s: bad signature", ExportHostAlloc)
	}
	ptr := uint32(results[0])
	if len(data) > 0 {
		if !m.Memory().Write(ptr, data) {
			return 0, fmt.Errorf("wasm plugin: guest ptr %d out of range", ptr)
		}
	}
	return ptr, nil
}

// hostOpResult 是宿主注入函数单次执行的产物。
type hostOpResult struct {
	raw json.RawMessage // ok 时的 result(可为 nil → 序列化为 null)
	b64 []byte          // ok 时的二进制 result(result_b64)
	err error           // 失败 → 信封 ok=false
}

func okRes(raw json.RawMessage) hostOpResult { return hostOpResult{raw: raw} }
func b64Res(b []byte) hostOpResult           { return hostOpResult{b64: b} }
func errRes(err error) hostOpResult          { return hostOpResult{err: err} }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// hostOp 是宿主注入函数表的一行:pairs = (ptr,len) 参数对的个数。
// op.run 全程处于实例互斥锁内(与 guest 调用同锁),不得再取锁。
type hostOp struct {
	name  string
	pairs int
	run   func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult
}

// hostOps 是宿主注入函数的完整表(顺序即 buildHostModule 的注册顺序)。
// 能力门控由 Surface 视图承担(plugSurface 语义:未声明能力拿到 no-op 或错误,与进程内插件一致)。
var hostOps = []hostOp{
	{name: ImportCall, pairs: 3, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		out, err := p.surface.Call(ctx, args[0], args[1], json.RawMessage(args[2]))
		if err != nil {
			return errRes(err)
		}
		return okRes(out)
	}},
	{name: ImportPlugins, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		return okRes(mustJSON(p.surface.Plugins()))
	}},
	{name: ImportGetPlugin, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		meta, found := p.surface.GetPlugin(args[0])
		return okRes(mustJSON(map[string]any{"found": found, "meta": meta}))
	}},
	{name: ImportGetConfig, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		v, found := p.surface.GetConfig(args[0], args[1])
		return okRes(mustJSON(map[string]any{"found": found, "value": v}))
	}},
	{name: ImportSetConfig, pairs: 3, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		if err := p.surface.SetConfig(args[0], args[1], json.RawMessage(args[2])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportGetSetting, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		v, ok := p.surface.GetSetting(args[0])
		return okRes(mustJSON(map[string]any{"found": ok, "value": v}))
	}},
	{name: ImportPersistRead, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surface.Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		data, err := fs.Read(args[0])
		if err != nil {
			return errRes(err)
		}
		return b64Res(data)
	}},
	{name: ImportPersistWrite, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surface.Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		if err := fs.Write(args[0], []byte(args[1])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportPersistList, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surface.Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		names, err := fs.List()
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(names))
	}},
	{name: ImportKVGet, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		kv, ok := p.surface.KV()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapKV))
		}
		v, found := kv.Get(args[0])
		if !found {
			return okRes(mustJSON(map[string]any{"found": false}))
		}
		return okRes(mustJSON(map[string]any{"found": true, "value_b64": base64.StdEncoding.EncodeToString(v)}))
	}},
	{name: ImportKVPut, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		kv, ok := p.surface.KV()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapKV))
		}
		kv.Put(args[0], []byte(args[1]))
		return okRes(nil)
	}},
	{name: ImportKVDelete, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		kv, ok := p.surface.KV()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapKV))
		}
		kv.Delete(args[0])
		return okRes(nil)
	}},
	{name: ImportKVKeys, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		kv, ok := p.surface.KV()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapKV))
		}
		return okRes(mustJSON(kv.Keys()))
	}},
	{name: ImportExec, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		out, err := p.surface.Exec(ctx, args[0])
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(map[string]any{"output": out}))
	}},
	{name: ImportOp, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 扩展能力走 Op 面:op 名 + 入参,守卫词与处理器由装配注入。
		out, err := p.surface.Op(ctx, args[0], json.RawMessage(args[1]))
		if err != nil {
			return errRes(err)
		}
		return okRes(out)
	}},
	{name: ImportPublishEvent, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 统一事件结构:入参是完整 contract.Event JSON(Type/SubType/Labels/Payload;Source 由框架回填)。
		var ev contract.Event
		if err := json.Unmarshal([]byte(args[0]), &ev); err != nil {
			return errRes(fmt.Errorf("bad event: %w", err))
		}
		p.surface.Publish(ctx, ev)
		return okRes(nil)
	}},
	{name: ImportSubscribeEvent, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 统一过滤:入参是完整 contract.EventFilter JSON(与远程通道/进程内同构)。
		var f contract.EventFilter
		if err := json.Unmarshal([]byte(args[0]), &f); err != nil {
			return errRes(fmt.Errorf("bad event filter: %w", err))
		}
		p.recordUnsub(p.surface.SubscribeEventFilter(&f, p.pushEvent))
		return okRes(nil)
	}},
	{name: ImportOnHook, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surface == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		p.surface.OnHook(args[0], p.invokeHook)
		return okRes(nil)
	}},
	{name: ImportReadAsset, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		data, ok := p.asset(args[0])
		if !ok {
			return errRes(fmt.Errorf("asset %q not found (only .egop.zip bundles assets/)", args[0]))
		}
		return b64Res(data)
	}},
	{name: ImportLog, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.logFn != nil {
			p.logFn(args[0], args[1]) // args = (level, msg)
		}
		return okRes(nil)
	}},
}

// buildHostModule 把宿主注入函数表实例化进 runtime(CompileModule 之前必须完成)。
func (p *Plugin) buildHostModule(ctx context.Context, r wazero.Runtime) error {
	b := r.NewHostModuleBuilder(HostModuleName)
	for _, op := range hostOps {
		params := make([]api.ValueType, 0, op.pairs*2)
		for range op.pairs {
			params = append(params, api.ValueTypeI32, api.ValueTypeI32)
		}
		run := op.run
		fb := b.NewFunctionBuilder().WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
				args := make([]string, 0, len(params)/2)
				for i := range len(params) / 2 {
					s, err := readGuestString(m.Memory(), uint32(stack[i*2]), uint32(stack[i*2+1]))
					if err != nil {
						s = ""
					}
					args = append(args, s)
				}
				res := run(ctx, m, p, args)
				env := envelope{OK: res.err == nil, Result: res.raw}
				if res.err != nil {
					env.Error = res.err.Error()
				} else if len(res.b64) > 0 {
					env.ResultB64 = base64.StdEncoding.EncodeToString(res.b64)
				}
				data, err := json.Marshal(env)
				if err != nil {
					data = []byte(`{"ok":false,"error":"host: marshal envelope"}`)
				}
				ptr, err := allocWriteGuest(ctx, m, data)
				if err != nil {
					stack[0] = 0 // 无信封位:guest 读到空串,按约定视为宿主故障
					return
				}
				stack[0] = pack(ptr, uint32(len(data)))
			}),
			params, []api.ValueType{api.ValueTypeI64})
		b = fb.Export(op.name)
	}
	_, err := b.Instantiate(ctx)
	return err
}
