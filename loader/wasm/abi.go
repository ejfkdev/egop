// wasm 插件 ABI 词汇与宿主注入函数表。
//
// 跨边界约定:
//   - 全部字符串 = guest linear memory 内的字节,(ptr,len) 成对传参(UTF-8 文本与
//     任意二进制都同一形状,不做转义);
//   - 宿主注入函数(module "egop")默认返回 i64 打包 (len<<32)|ptr,指向经 guest
//     导出 egop_host_alloc 分配的结果信封
//     {"ok":bool,"result":any,"result_b64":string,"error":string}:ok=false 时
//     error 为人类可读消息;文件/KV 等二进制值走 result_b64(base64)。
//   - **二进制通道**(rawParts):net_request_raw / net_body_read_raw /
//     fs_read_raw / persist_read_raw / kv_get_raw 直接传/返原始字节,不包
//     JSON、不做 base64——i64 即 (ptr,len),读结果统一帧
//     [u32 status][u32 len][payload](0=数据/1=空或未命中/2=错误文本)。
//     这是基础能力:文本/二进制同一通道,避免大请求/流式响应/大文件在 ABI
//     边界上的 33%+ 编码与多次拷贝;旧 import 保留,旧 guest 不受影响。
//   - guest 导出函数除 egop_meta(裸 JSON)与 egop_on_event(事件推送回调,无返回)
//     外,一律返回同一信封格式的 (ptr,len)。
package wasm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	// ExportToolSpecs 活体工具面查询(可选导出;动态工具插件如 MCP 客户端的
	// 运行期发现真源)。缺省/失败=回落线上清单静态 Tools(向后兼容:旧 guest
	// 无此导出照常工作)。
	ExportToolSpecs = "egop_tool_specs"
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
	// ImportPersistWrite Surface.Persist:写文件(整文件覆盖语义)。
	ImportPersistWrite = "persist_write"
	// ImportPersistAppend Surface.Persist:末尾追加(append-only 日志真源;
	// 拿 persist_write 追加=每次截断只留末行)。
	ImportPersistAppend = "persist_append"
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
	// ImportKVUpdateBegin/End 是附加 KV 事务 ABI:begin 在当前模块登记
	// backend Update 视图,期间普通 kv_* 复用它,end 释放 backend 锁。
	// 旧 guest 不引用这些 import;不支持 Update 的 backend 在 begin 明确报错。
	ImportKVUpdateBegin = "kv_update_begin"
	ImportKVUpdateEnd   = "kv_update_end"
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
	// ImportFSRead Surface.FS:读全局文件系统(需 fs.read;结果走 result_b64)。
	ImportFSRead = "fs_read"
	// ImportFSStat / ImportFSReadRange / ImportFSAppend 是 FS 的范围扩展:
	// 大文件按 4MiB 块分页读/追加,不整文件过 ABI(FSEx 缺席=不支持)。
	ImportFSStat      = "fs_stat"
	ImportFSReadRange = "fs_read_range"
	ImportFSAppend    = "fs_append"
	// MaxFSReadBytes 是 fs_read 单次回传 guest 的数据上限(8MiB):base64 信封
	// 放大 4/3 + guest 解析复制,8MiB 数据在 64MiB 线性内存里安全;超限=宿主侧
	// 报错(工具语义层各自再收紧,如 fsp grep 256KB)。
	MaxFSReadBytes = 8 << 20
	// MaxFSRangeBytes 是 fs_read_range/fs_append 的单次块上限(4MiB):调用方分页,
	// 单块常驻内存有界;整文件大小不再受此限制。
	MaxFSRangeBytes = 4 << 20
	// MaxNetBodyBytes 是 net_request/net_request_raw 请求体的 ABI 护栏(32MiB):超过多半是
	// 上游压缩/预算失效,整包塞进 guest 会连同解析拷贝打爆线性内存。**必须高于**
	// 宿主 LLM 请求阀(16MiB)+ provider 包装开销:否则贴着阀上限的请求会在
	// provider body 层被误拒(2026-11 16MiB 测量实锤 16777310 > 16777216)。
	MaxNetBodyBytes = 32 << 20
	// MaxBinaryReadBytes 是 persist/kv 原始读的护栏(32MiB):状态/队列文件正常
	// 在数百 KB;超大值直接拒绝比让 guest OOM 更可控。
	MaxBinaryReadBytes = 32 << 20

	// ImportFSReadDir Surface.FS:列举目录一层(需 fs.read;结果=JSON 数组 [{name,is_dir,size?,mod_time?}])。
	ImportFSReadDir = "fs_readdir"
	// ImportFSWrite Surface.FS:写全局文件系统(需 fs.write;入参 (name,data) 字符串对)。
	ImportFSWrite = "fs_write"
	// ImportLog 日志直出:经 Options.LogFn 接入宿主日志面(nil = 静默)。
	ImportLog = "log"
	// ImportNetRequest Surface.Net:发起一次 HTTP(S) 请求(入参=编组请求 JSON;
	// result=状态/头/流式 body 句柄)。响应 body 经 ImportNetBodyRead 增量读。
	ImportNetRequest = "net_request"
	// ImportNetBodyRead Surface.Net:读流式响应 body 的一段字节(chunk 走 result_b64 信封)。
	ImportNetBodyRead = "net_body_read"
	// ImportNetBodyClose Surface.Net:关闭并释放 body 句柄。
	ImportNetBodyClose = "net_body_close"

	// ImportMemPages 返回本实例生效的线性内存上限(页,64KiB),供 guest 自适应
	// Go 软堆顶(硬顶的 3/4),在重插件声明更高额度时自动放宽 GC 目标。
	ImportMemPages = "memory_pages"

	// ImportNetRequestRaw 是 net_request 的**二进制通道**版本:请求体作为第 4 个
	// (ptr,len) 参数原样过边界(不再 base64 塞进 JSON),响应头/句柄仍走小信封。
	ImportNetRequestRaw = "net_request_raw"
	// ImportFSReadRaw / ImportPersistReadRaw / ImportKVGetRaw 是二进制通道的
	// 文件/持久/KV 读版本:帧 [status][len][bytes](KV status 1=not found)。
	ImportFSReadRaw      = "fs_read_raw"
	ImportPersistReadRaw = "persist_read_raw"
	ImportKVGetRaw       = "kv_get_raw"
	// ImportNetBodyReadRaw 是 net_body_read 的二进制通道版本:返回帧
	// [u32 status][u32 len][payload](status 0=数据/1=EOF/2=错误文本),
	// 每块响应字节零 base64、零 JSON 转义。
	ImportNetBodyReadRaw = "net_body_read_raw"

	// ManifestSection 是 .egop.wasm 内置清单的自定义段名。
	ManifestSection = "egop.manifest"

	// SuffixWasm / SuffixZip 是插件文件命名后缀约定(*.egop.wasm / *.egop.zip)。
	// 消费方的其它后缀约定(自有品牌 zip 等)经 Options.ExtraSuffixes 装配注入——
	// 品牌词不进内容无关库。
	SuffixWasm = ".egop.wasm"
	SuffixZip  = ".egop.zip"
)

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

// allocGuest 经 guest 导出的 egop_host_alloc 在 guest 内存分配 n 字节,返回指针。
func allocGuest(ctx context.Context, m api.Module, n uint32) (uint32, error) {
	fn := m.ExportedFunction(ExportHostAlloc)
	if fn == nil {
		return 0, fmt.Errorf("wasm plugin: export %q missing", ExportHostAlloc)
	}
	results, err := fn.Call(ctx, uint64(n))
	if err != nil {
		return 0, fmt.Errorf("wasm plugin: %s: %w", ExportHostAlloc, err)
	}
	if len(results) != 1 {
		return 0, fmt.Errorf("wasm plugin: %s: bad signature", ExportHostAlloc)
	}
	return uint32(results[0]), nil
}

// writeGuestParts 一次分配 total 字节,按序写入分段(不拼接中间缓冲)。
func writeGuestParts(ctx context.Context, m api.Module, parts [][]byte) (ptr, total uint32, err error) {
	for _, p := range parts {
		total += uint32(len(p))
	}
	ptr, err = allocGuest(ctx, m, total)
	if err != nil {
		return 0, 0, err
	}
	off := uint32(0)
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		if !m.Memory().Write(ptr+off, p) {
			return 0, 0, fmt.Errorf("wasm plugin: guest ptr %d out of range", ptr+off)
		}
		off += uint32(len(p))
	}
	return ptr, total, nil
}

// allocWriteGuest 经 guest 导出的 egop_host_alloc 在 guest 内存落 data,返回指针。
func allocWriteGuest(ctx context.Context, m api.Module, data []byte) (uint32, error) {
	ptr, _, err := writeGuestParts(ctx, m, [][]byte{data})
	return ptr, err
}

// hostOpResult 是宿主注入函数单次执行的产物。
type hostOpResult struct {
	raw json.RawMessage // ok 时的 result(可为 nil → 序列化为 null)
	b64 []byte          // ok 时的二进制 result(result_b64)
	// rawParts 非 nil 时**不走信封**:分段字节按下标顺序写入 guest 内存,
	// i64 直接返回 (ptr,len)。二进制通道用——分两段(帧头/数据)可避免为
	// 每个分块再复制一份"帧+负载"的整块。
	rawParts [][]byte
	err      error // 失败 → 信封 ok=false
}

func okRes(raw json.RawMessage) hostOpResult  { return hostOpResult{raw: raw} }
func b64Res(b []byte) hostOpResult            { return hostOpResult{b64: b} }
func rawPartsRes(parts [][]byte) hostOpResult { return hostOpResult{rawParts: parts} }

// rawFrame 构造二进制通道帧 [u32 status][u32 len][payload],按两段返回:
// 8 字节头 + 负载(负载不复制,直接由 guest 写入目标内存)。
// status: 0=数据, 1=空/未找到/EOF, 2=错误文本(随帧返回而非信封)。
func rawFrame(status uint32, payload []byte) [][]byte {
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], status)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	return [][]byte{hdr, payload}
}
func errRes(err error) hostOpResult { return hostOpResult{err: err} }

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

// runHostOpDecoded 先完整解码参数再执行操作。任何无效 guest 指针都直接
// 返回错误,不得把该参数替换成空串后触发空键/空路径操作。
func runHostOpDecoded(ctx context.Context, m api.Module, p *Plugin, op hostOp, stack []uint64) hostOpResult {
	args := make([]string, 0, op.pairs)
	for i := range op.pairs {
		if len(stack) < i*2+2 {
			return errRes(fmt.Errorf("wasm plugin %s: %s: incomplete argument stack", p.name, op.name))
		}
		s, err := readGuestString(m.Memory(), uint32(stack[i*2]), uint32(stack[i*2+1]))
		if err != nil {
			return errRes(fmt.Errorf("wasm plugin %s: %s: %w", p.name, op.name, err))
		}
		args = append(args, s)
	}
	return op.run(ctx, m, p, args)
}

// legacyBase64DecodedLen 计算合法 StdEncoding 输入的实际解码长度,忽略其
// 解码器同样忽略的 CR/LF;调用方可在分配解码缓冲前执行尺寸护栏。
func legacyBase64DecodedLen(s string) int {
	encodedLen := len(s)
	last, previous := -1, -1
	for i := range len(s) {
		if s[i] == '\r' || s[i] == '\n' {
			encodedLen--
			continue
		}
		previous, last = last, i
	}
	decoded := base64.StdEncoding.DecodedLen(encodedLen)
	if last >= 0 && s[last] == '=' {
		decoded--
		if previous >= 0 && s[previous] == '=' {
			decoded--
		}
	}
	return decoded
}

// kvForModule 在事务期间返回当前模块的 Update 视图，否则按旧路径新建/获取
// 插件 KV。hostOp 全程持 inst.mu，故读取 inst.kvTx 不需要额外加锁。
func (p *Plugin) kvForModule(m api.Module) (contract.KeyValue, error) {
	if p.surf() == nil {
		return nil, fmt.Errorf("plugin %s: surface not wired", p.name)
	}
	if i := p.instForModule(m); i != nil && i.kvTx != nil {
		return i.kvTx.view, nil
	}
	kv, ok := p.surf().KV()
	if !ok {
		return nil, fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapKV)
	}
	return kv, nil
}

// hostOps 是宿主注入函数的完整表(顺序即 buildHostModule 的注册顺序)。
// 能力门控由 Surface 视图承担(plugSurface 语义:未声明能力拿到 no-op 或错误,与进程内插件一致)。
var hostOps = []hostOp{
	{name: ImportCall, pairs: 3, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		out, err := p.surf().Call(ctx, args[0], args[1], json.RawMessage(args[2]))
		if err != nil {
			return errRes(err)
		}
		return okRes(out)
	}},
	{name: ImportMemPages, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		return okRes(mustJSON(p.memPages))
	}},
	{name: ImportPlugins, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		return okRes(mustJSON(p.surf().Plugins()))
	}},
	{name: ImportGetPlugin, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		meta, found := p.surf().GetPlugin(args[0])
		return okRes(mustJSON(map[string]any{"found": found, "meta": meta}))
	}},
	{name: ImportGetConfig, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		v, found := p.surf().GetConfig(args[0], args[1])
		return okRes(mustJSON(map[string]any{"found": found, "value": v}))
	}},
	{name: ImportSetConfig, pairs: 3, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		if err := p.surf().SetConfig(args[0], args[1], json.RawMessage(args[2])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportGetSetting, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		v, ok := p.surf().GetSetting(args[0])
		return okRes(mustJSON(map[string]any{"found": ok, "value": v}))
	}},
	{name: ImportPersistRead, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surf().Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		data, err := fs.Read(args[0])
		if err != nil {
			return errRes(err)
		}
		if len(data) > MaxBinaryReadBytes {
			return errRes(fmt.Errorf("persist read %s: %d bytes exceeds ABI guard %d (guest memory protection)", args[0], len(data), MaxBinaryReadBytes))
		}
		return b64Res(data)
	}},
	{name: ImportPersistReadRaw, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return rawPartsRes(rawFrame(2, []byte("surface not wired")))
		}
		fs, ok := p.surf().Persist()
		if !ok {
			return rawPartsRes(rawFrame(2, []byte("capability "+contract.CapPersist+" not available")))
		}
		data, err := fs.Read(args[0])
		if err != nil {
			return rawPartsRes(rawFrame(2, []byte(err.Error())))
		}
		if len(data) > MaxBinaryReadBytes {
			return rawPartsRes(rawFrame(2, []byte(fmt.Sprintf("persist read %s: %d bytes exceeds ABI guard %d", args[0], len(data), MaxBinaryReadBytes))))
		}
		return rawPartsRes(rawFrame(0, data))
	}},
	{name: ImportPersistWrite, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surf().Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		if err := fs.Write(args[0], []byte(args[1])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportPersistAppend, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surf().Persist()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapPersist))
		}
		if err := fs.Append(args[0], []byte(args[1])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportPersistList, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fs, ok := p.surf().Persist()
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
		kv, err := p.kvForModule(m)
		if err != nil {
			return errRes(err)
		}
		v, found := kv.Get(args[0])
		if !found {
			return okRes(mustJSON(map[string]any{"found": false}))
		}
		if len(v) > MaxBinaryReadBytes {
			return errRes(fmt.Errorf("kv get %s: %d bytes exceeds ABI guard %d (guest memory protection)", args[0], len(v), MaxBinaryReadBytes))
		}
		return okRes(mustJSON(map[string]any{"found": true, "value_b64": base64.StdEncoding.EncodeToString(v)}))
	}},
	{name: ImportKVGetRaw, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		kv, err := p.kvForModule(m)
		if err != nil {
			return rawPartsRes(rawFrame(2, []byte(err.Error())))
		}
		v, found := kv.Get(args[0])
		if !found {
			return rawPartsRes(rawFrame(1, nil)) // 1 = not found
		}
		if len(v) > MaxBinaryReadBytes {
			return rawPartsRes(rawFrame(2, []byte(fmt.Sprintf("kv get %s: %d bytes exceeds ABI guard %d", args[0], len(v), MaxBinaryReadBytes))))
		}
		return rawPartsRes(rawFrame(0, v))
	}},
	{name: ImportKVPut, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		kv, err := p.kvForModule(m)
		if err != nil {
			return errRes(err)
		}
		kv.Put(args[0], []byte(args[1]))
		return okRes(nil)
	}},
	{name: ImportKVDelete, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		kv, err := p.kvForModule(m)
		if err != nil {
			return errRes(err)
		}
		kv.Delete(args[0])
		return okRes(nil)
	}},
	{name: ImportKVKeys, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		kv, err := p.kvForModule(m)
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(kv.Keys()))
	}},
	{name: ImportKVUpdateBegin, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		kv, err := p.kvForModule(m)
		if err != nil {
			return errRes(err)
		}
		i := p.instForModule(m)
		if i == nil {
			return errRes(fmt.Errorf("plugin %s: kv update: unknown module instance", p.name))
		}
		if _, err := i.beginKVUpdate(kv); err != nil {
			return errRes(fmt.Errorf("plugin %s: %w", p.name, err))
		}
		return okRes(nil)
	}},
	{name: ImportKVUpdateEnd, pairs: 0, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		i := p.instForModule(m)
		if i == nil || i.kvTx == nil {
			return errRes(fmt.Errorf("plugin %s: kv update: no active transaction", p.name))
		}
		if err := i.endKVUpdate(); err != nil {
			return errRes(fmt.Errorf("plugin %s: kv update end: %w", p.name, err))
		}
		return okRes(nil)
	}},
	{name: ImportExec, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		out, err := p.surf().Exec(ctx, args[0])
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(map[string]any{"output": out}))
	}},
	{name: ImportOp, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 扩展能力走 Op 面:op 名 + 入参,守卫词与处理器由装配注入。
		out, err := p.surf().Op(ctx, args[0], json.RawMessage(args[1]))
		if err != nil {
			return errRes(err)
		}
		return okRes(out)
	}},
	{name: ImportPublishEvent, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 统一事件结构:入参是完整 contract.Event JSON(Type/SubType/Labels/Payload;Source 由框架回填)。
		var ev contract.Event
		if err := json.Unmarshal([]byte(args[0]), &ev); err != nil {
			return errRes(fmt.Errorf("bad event: %w", err))
		}
		p.surf().Publish(ctx, ev)
		return okRes(nil)
	}},
	{name: ImportSubscribeEvent, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// 统一过滤:入参是完整 contract.EventFilter JSON(与远程通道/进程内同构)。
		// 订阅钉主实例:池>1 时次实例的订阅按无操作忽略(观察面语义不随池放大)。
		var f contract.EventFilter
		if err := json.Unmarshal([]byte(args[0]), &f); err != nil {
			return errRes(fmt.Errorf("bad event filter: %w", err))
		}
		if p.registerOnPrimary(m) {
			if i := p.instForModule(m); i != nil {
				i.unsubs.Defer(p.surf().SubscribeEventFilter(&f, p.pushEvent))
			}
		} else if p.logFn != nil {
			p.logFn("warn", "wasm plugin "+p.name+": subscribe from secondary instance ignored (observation plane pinned to primary)")
		}
		return okRes(nil)
	}},
	{name: ImportOnHook, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		// hook 注册钉主实例,且撤销记进主实例 unsubs——revive 重放 init 时先撤销
		// 旧注册再重挂,不随复活次数累积(订阅侧同理)。
		if p.registerOnPrimary(m) {
			if i := p.instForModule(m); i != nil {
				i.unsubs.Defer(p.surf().OnHook(args[0], p.invokeHook))
			}
		} else if p.logFn != nil {
			p.logFn("warn", "wasm plugin "+p.name+": on_hook from secondary instance ignored (observation plane pinned to primary)")
		}
		return okRes(nil)
	}},
	{name: ImportReadAsset, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		data, ok := p.asset(args[0])
		if !ok {
			return errRes(fmt.Errorf("asset %q not found (only .egop.zip bundles assets/)", args[0]))
		}
		return b64Res(data)
	}},
	{name: ImportFSRead, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapFSRead))
		}
		data, err := fsys.ReadFile(args[0])
		if err != nil {
			return errRes(err)
		}
		// ABI 级尺寸护栏:base64 信封会把数据放大 4/3 倍灌进 guest 线性内存
		// (上限 64MiB),巨文件在宿主侧拒读才是单点防线——guest 侧任何预筛都
		// 防不住 symlink(Lstat 尺寸小、ReadFile 跟随链接读到巨目标;2026-09-10
		// grep /tmp 撞 34MB 固件 ftab.bin OOM trap 实锤)。
		if len(data) > MaxFSReadBytes {
			return errRes(fmt.Errorf("fs read %s: %d bytes exceeds ABI guard %d (guest memory protection)", args[0], len(data), MaxFSReadBytes))
		}
		return b64Res(data)
	}},
	{name: ImportFSStat, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapFSRead))
		}
		ex, ok := fsys.(contract.FSEx)
		if !ok {
			return errRes(fmt.Errorf("fs stat: not supported by this FS"))
		}
		st, err := ex.Stat(args[0])
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(st))
	}},
	{name: ImportFSReadRange, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return rawPartsRes(rawFrame(2, []byte("surface not wired")))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return rawPartsRes(rawFrame(2, []byte("capability "+contract.CapFSRead+" not available")))
		}
		ex, ok := fsys.(contract.FSEx)
		if !ok {
			return rawPartsRes(rawFrame(2, []byte("fs range read: not supported by this FS")))
		}
		var q struct {
			Offset int64 `json:"offset"`
			Len    int64 `json:"len"`
		}
		if err := json.Unmarshal([]byte(args[1]), &q); err != nil {
			return rawPartsRes(rawFrame(2, []byte("bad range query: "+err.Error())))
		}
		if q.Offset < 0 || q.Len < 0 {
			return rawPartsRes(rawFrame(2, []byte("bad range: negative offset/len")))
		}
		if q.Len > MaxFSRangeBytes {
			return rawPartsRes(rawFrame(2, []byte(fmt.Sprintf("range len %d exceeds per-call cap %d (page in chunks)", q.Len, MaxFSRangeBytes))))
		}
		data, err := ex.ReadRange(args[0], q.Offset, q.Len)
		if err != nil {
			return rawPartsRes(rawFrame(2, []byte(err.Error())))
		}
		return rawPartsRes(rawFrame(0, data))
	}},
	{name: ImportFSAppend, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapFSWrite))
		}
		ex, ok := fsys.(contract.FSEx)
		if !ok {
			return errRes(fmt.Errorf("fs append: not supported by this FS"))
		}
		if len(args[1]) > MaxFSRangeBytes {
			return errRes(fmt.Errorf("fs append %d bytes exceeds per-call cap %d (write in chunks)", len(args[1]), MaxFSRangeBytes))
		}
		if err := ex.Append(args[0], []byte(args[1])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportFSReadRaw, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return rawPartsRes(rawFrame(2, []byte("surface not wired")))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return rawPartsRes(rawFrame(2, []byte("capability "+contract.CapFSRead+" not available")))
		}
		data, err := fsys.ReadFile(args[0])
		if err != nil {
			return rawPartsRes(rawFrame(2, []byte(err.Error())))
		}
		if len(data) > MaxFSReadBytes {
			return rawPartsRes(rawFrame(2, []byte(fmt.Sprintf("fs read %s: %d bytes exceeds ABI guard %d (guest memory protection)", args[0], len(data), MaxFSReadBytes))))
		}
		return rawPartsRes(rawFrame(0, data))
	}},
	{name: ImportFSReadDir, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapFSRead))
		}
		entries, err := fsys.ReadDir(args[0])
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(entries))
	}},
	{name: ImportFSWrite, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		fsys, ok := p.surf().FS()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapFSWrite))
		}
		if err := fsys.WriteFile(args[0], []byte(args[1])); err != nil {
			return errRes(err)
		}
		return okRes(nil)
	}},
	{name: ImportLog, pairs: 2, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.logFn != nil {
			p.logFn(args[0], args[1]) // args = (level, msg)
		}
		return okRes(nil)
	}},
	{name: ImportNetRequest, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		net, ok := p.surf().Net()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapNet))
		}
		var req struct {
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body_b64"` // base64 请求体(整包;流式请求体不跨边界)
		}
		if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
			return errRes(fmt.Errorf("bad net request: %w", err))
		}
		var body io.Reader
		if req.Body != "" {
			if n := legacyBase64DecodedLen(req.Body); n > MaxNetBodyBytes {
				return errRes(fmt.Errorf("net request body %d bytes exceeds ABI guard %d (guest memory protection)", n, MaxNetBodyBytes))
			}
			b, err := base64.StdEncoding.DecodeString(req.Body)
			if err != nil {
				return errRes(fmt.Errorf("bad net request body: %w", err))
			}
			if len(b) > MaxNetBodyBytes {
				return errRes(fmt.Errorf("net request body %d bytes exceeds ABI guard %d (guest memory protection)", len(b), MaxNetBodyBytes))
			}
			body = bytes.NewReader(b)
		}
		resp, err := net.Request(ctx, contract.Request{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: body})
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(map[string]any{
			"status":      resp.Status,
			"headers":     resp.Headers,
			"trailers":    resp.Trailers,
			"body_handle": p.instForModule(m).netAlloc(resp.Body),
		}))
	}},
	{name: ImportNetBodyRead, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		var body io.Reader
		if i := p.instForModule(m); i != nil {
			body = i.netGet(args[0])
		}
		if body != nil {
			buf := make([]byte, 32*1024)
			n, err := body.Read(buf)
			if n > 0 {
				return okRes(mustJSON(map[string]any{"chunk_b64": base64.StdEncoding.EncodeToString(buf[:n])}))
			}
			if errors.Is(err, io.EOF) {
				return okRes(mustJSON(map[string]any{"eof": true}))
			}
			if err != nil {
				return errRes(err)
			}
			// 0,nil:net/http body 不会这样;保守视为流结束。
			return okRes(mustJSON(map[string]any{"eof": true}))
		}
		// 未知/已关句柄:返回 eof,让 guest 侧 Read 得到 io.EOF 收尾。
		return okRes(mustJSON(map[string]any{"eof": true}))
	}},
	{name: ImportNetBodyClose, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if i := p.instForModule(m); i != nil {
			i.netClose(args[0])
		}
		return okRes(nil)
	}},
	// ---- 二进制通道(net) ----
	{name: ImportNetRequestRaw, pairs: 4, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		if p.surf() == nil {
			return errRes(fmt.Errorf("plugin %s: surface not wired", p.name))
		}
		net, ok := p.surf().Net()
		if !ok {
			return errRes(fmt.Errorf("plugin %s: capability %q not available", p.name, contract.CapNet))
		}
		if len(args[3]) > MaxNetBodyBytes {
			return errRes(fmt.Errorf("net request body %d bytes exceeds ABI guard %d (guest memory protection)", len(args[3]), MaxNetBodyBytes))
		}
		hdrs := map[string]string{}
		if args[2] != "" {
			if err := json.Unmarshal([]byte(args[2]), &hdrs); err != nil {
				return errRes(fmt.Errorf("bad net headers: %w", err))
			}
		}
		var body io.Reader
		if len(args[3]) > 0 {
			body = strings.NewReader(args[3]) // 原始字节,零拷贝(不再转 []byte)
		}
		resp, err := net.Request(ctx, contract.Request{Method: args[0], URL: args[1], Headers: hdrs, Body: body})
		if err != nil {
			return errRes(err)
		}
		return okRes(mustJSON(map[string]any{
			"status":      resp.Status,
			"headers":     resp.Headers,
			"trailers":    resp.Trailers,
			"body_handle": p.instForModule(m).netAlloc(resp.Body),
		}))
	}},
	{name: ImportNetBodyReadRaw, pairs: 1, run: func(ctx context.Context, m api.Module, p *Plugin, args []string) hostOpResult {
		frame := func(status uint32, payload []byte) hostOpResult {
			return rawPartsRes(rawFrame(status, payload))
		}
		var body io.Reader
		if i := p.instForModule(m); i != nil {
			body = i.netGet(args[0])
		}
		if body == nil {
			return frame(1, nil) // 未知/已关句柄:EOF
		}
		buf := make([]byte, 32*1024)
		n, err := body.Read(buf)
		if n > 0 {
			return frame(0, buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return frame(1, nil)
		}
		if err != nil {
			return frame(2, []byte(err.Error()))
		}
		return frame(1, nil)
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
		fb := b.NewFunctionBuilder().WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
				res := runHostOpDecoded(ctx, m, p, op, stack)
				if len(res.rawParts) > 0 && res.err == nil {
					// 二进制通道:分段字节直接落 guest 内存,不包 JSON 信封。
					ptr, n, aerr := writeGuestParts(ctx, m, res.rawParts)
					if aerr != nil {
						stack[0] = 0
						return
					}
					stack[0] = pack(ptr, n)
					return
				}
				env := envelope{OK: res.err == nil, Result: res.raw}
				if res.err != nil {
					env.Error = res.err.Error()
					if detail, ok := contract.ErrorDetailOf(res.err); ok {
						env.ErrorDetail = &detail
					}
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
