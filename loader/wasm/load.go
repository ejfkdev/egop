// 插件包加载器:*.egop.wasm(自定义段 egop.manifest 内置清单,缺省回退 egop_meta
// 导出)与 *.egop.zip(manifest.json + plugin.wasm + assets/)两种形态,加目录
// 递归发现 ScanDir 与设置键解析 ParseDirs。
package wasm

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	wasi_snapshot_preview1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// DefaultMaxPages 是 guest 内存上限的默认值(页,每页 64KiB → 64MiB)。
// DefaultMaxPages 1024 页(64MiB)在重插件上不够:llm/react(自举 wasm)的
// Go runtime 堆+JSON 序列化缓冲随会话史增长,大史会话 encoding/json growslice
// 直接 allocLarge unreachable(2026-10-12 TSec 跑分 live 实锤,且 llm OOM 后
// 压缩通道同死=无法自救)。4096 页(256MiB)=预留上限,线性内存按需 commit,
// 调大不预占 RSS;轻插件不受影响(上限只是天花板)。
const DefaultMaxPages uint32 = 4096

// MaxPagesCeiling 是 egop.memory_pages 清单扩展可申请的上限(16384 页 = 1GiB)。
// 线性内存按需 commit,提高上限只增加虚拟地址空间;重插件(llm/react)在清单里
// 声明更高额度以容纳长请求/长输出,轻插件保持默认。
const MaxPagesCeiling uint32 = 16384

// sharedCompilationCache 进程级 wazero 编译缓存。
//
// 文件后端默认关闭(不落盘):插件是 6MB Go wasm,冷编译约 1s/个、几十个
// 插件=50s 级 boot;文件缓存实测 56s→22s,但缓存目录约 1.1GB。是否用磁盘
// 换启动时间由部署方决定——设 EGOP_WASM_CACHE_DIR=<dir> 即启用(目录由
// wazero 管理,可直接删除;插件内容/Wazero 版本变会按 hash 自动失效)。
// 未设/不可写时用进程内内存缓存(同一模块多实例仍复用编译结果)。
var (
	compCacheOnce sync.Once
	compCache     wazero.CompilationCache
)

func sharedCompilationCache() wazero.CompilationCache {
	compCacheOnce.Do(func() {
		dir := strings.TrimSpace(os.Getenv("EGOP_WASM_CACHE_DIR"))
		if dir != "" {
			if c, err := wazero.NewCompilationCacheWithDir(dir); err == nil {
				compCache = c
				return
			}
		}
		compCache = wazero.NewCompilationCache()
	})
	return compCache
}

// Options 是加载选项(零值可用)。
type Options struct {
	MaxMemoryPages uint32 // guest 内存上限页数;0 = DefaultMaxPages
	// LogFn 接 guest 日志(ABI log(level,msg) 注入函数):nil = 静默。
	LogFn func(level, msg string)
	// ExtraSuffixes 追加的 zip 包后缀(小写比较、须以 "." 开头,如 ".brand.zip"):
	// 内容布局与 *.egop.zip 完全相同(manifest.json + plugin.wasm(可选)+ assets/)。
	// 品牌/项目自有后缀由装配注入——内容无关库不内置任何业务词。
	ExtraSuffixes []string
	// MaxEntryBytes / MaxTotalBytes 是 zip 解压上限(防 zip bomb;插件是不可信
	// 输入)。0 = 默认(DefaultMaxEntryBytes / DefaultMaxTotalBytes)。
	MaxEntryBytes int64
	MaxTotalBytes int64
	// MaxEntries 是 ZIP 总条目上限(含目录项);0 = DefaultMaxEntries。
	MaxEntries int
}

// AssetInfo 是暂存包检查时暴露的资产元数据;Name 与 Plugin.Assets 的键一致,
// 不含 "assets/" 前缀。SHA256 供上传方做完整性展示或去重,不暴露资产字节。
type AssetInfo struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PackageInspection 是 ZIP 包的静态检查结果。EntryCount 含目录项,TotalBytes
// 是全部文件项的实际解压字节数;WASMSize 单独给出 plugin.wasm 大小。
type PackageInspection struct {
	Manifest   contract.Manifest `json:"manifest"`
	Assets     []AssetInfo       `json:"assets"`
	WASMSize   int64             `json:"wasm_size"`
	EntryCount int               `json:"entry_count"`
	TotalBytes int64             `json:"total_bytes"`
}

// zip 解压默认上限:单条目 256MiB、整包聚合 1GiB、4096 条(正常插件包远小于此;
// 恶意高压缩比包在触顶时立即拒载,不被读进内存)。
const (
	DefaultMaxEntryBytes int64 = 256 << 20
	DefaultMaxTotalBytes int64 = 1 << 30
	DefaultMaxEntries          = 4096
)

// IsPluginFile 判定文件名是否插件包(内置 *.egop.wasm / *.egop.zip + extra 追加
// zip 后缀;大小写不敏感)。扫描侧(ScanFS/autoload)与装载侧(LoadFS)共用同一
// 判定,后缀集合只在这里收敛一次。
func IsPluginFile(name string, extra []string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, SuffixWasm) {
		return true
	}
	return isZipName(lower, extra)
}

// isZipName 判定(已小写的)名字是否 zip 包形态:内置 .egop.zip 或 extra 注入后缀。
func isZipName(lower string, extra []string) bool {
	if strings.HasSuffix(lower, SuffixZip) {
		return true
	}
	for _, s := range extra {
		if s != "" && strings.HasSuffix(lower, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// InspectPackage 静态检查暂存的 ZIP 插件包,返回清单与资产元数据。与 LoadFS
// 不同,它要求 plugin.wasm 存在,并且只编译模块:每次使用全新的 runtime 和内存
// 编译缓存,绝不实例化模块、调用导出或执行 start 函数,也不读取共享/磁盘缓存。
// name 只用于错误定位,ZIP 后缀由调用方按自己的上传策略判定。
func InspectPackage(ctx context.Context, data []byte, name string, opts Options) (*PackageInspection, error) {
	if name == "" {
		name = "plugin.zip"
	}
	pkg, err := parseZipPackage(data, opts)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %w", name, err)
	}
	if pkg.wasm == nil {
		return nil, fmt.Errorf("wasm plugin %s: zip missing plugin.wasm (required for inspection)", name)
	}

	p := newPlugin(name)
	p.manifest = pkg.manifest
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	rcfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(p.memoryPages(opts)).WithCompilationCache(cache)
	r := wazero.NewRuntimeWithConfig(ctx, rcfg)
	defer func() { _ = r.Close(context.Background()) }()
	compiled, err := r.CompileModule(ctx, pkg.wasm)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: compile: %w", name, err)
	}
	defer func() { _ = compiled.Close(context.Background()) }()
	if err := validateHostImports(name, compiled.ImportedFunctions()); err != nil {
		return nil, err
	}
	if err := validateStructuralABI(name, pkg.manifest, compiled.ExportedFunctions(), compiled.ExportedMemories()); err != nil {
		return nil, err
	}

	out := &PackageInspection{
		Manifest:   pkg.manifest,
		WASMSize:   int64(len(pkg.wasm)),
		EntryCount: pkg.entryCount,
		TotalBytes: pkg.totalBytes,
		Assets:     make([]AssetInfo, 0, len(pkg.assets)),
	}
	names := make([]string, 0, len(pkg.assets))
	for assetName := range pkg.assets {
		names = append(names, assetName)
	}
	sort.Strings(names)
	for _, assetName := range names {
		raw := pkg.assets[assetName]
		sum := sha256.Sum256(raw)
		out.Assets = append(out.Assets, AssetInfo{Name: assetName, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

// LoadFile 按路径加载插件包(name 取文件名以判定包形态)。
func LoadFile(ctx context.Context, path string, opts Options) (*Plugin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadFS(ctx, data, filepath.Base(path), opts)
}

// LoadFS 加载插件包字节(形态由文件名后缀判定:*.egop.zip、*.egop.wasm 或
// Options.ExtraSuffixes 注入的 zip 后缀)。
func LoadFS(ctx context.Context, data []byte, name string, opts Options) (*Plugin, error) {
	if name == "" {
		name = "plugin"
	}
	lower := strings.ToLower(name)
	if isZipName(lower, opts.ExtraSuffixes) {
		mf, wasmBytes, assets, err := parseZip(data, opts)
		if err != nil {
			return nil, fmt.Errorf("wasm plugin %s: %w", name, err)
		}
		p := newPlugin(name)
		p.manifest = mf
		p.assets = assets
		if wasmBytes == nil {
			// 无代码插件(纯清单/资产):不实例化 guest,仅做清单校验。
			if err := p.validateCodeless(); err != nil {
				return nil, err
			}
			return p, nil
		}
		if err := p.instantiate(ctx, wasmBytes, opts, p.poolSize()); err != nil {
			return nil, err
		}
		if err := p.validate(); err != nil {
			_ = p.Close(ctx)
			return nil, err
		}
		return p, nil
	}
	if !strings.HasSuffix(lower, SuffixWasm) {
		return nil, fmt.Errorf("wasm plugin %s: unsupported suffix (want %s, %s or an injected extra suffix)", name, SuffixWasm, SuffixZip)
	}
	mf, found, err := parseCustomManifest(data)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %w", name, err)
	}
	p := newPlugin(name)
	if found {
		p.manifest = mf
	}
	if err := p.instantiate(ctx, data, opts, 1); err != nil {
		return nil, err
	}
	if !found {
		raw, err := p.callMeta(ctx)
		if err != nil {
			_ = p.Close(ctx)
			return nil, fmt.Errorf("wasm plugin %s: manifest section absent and %s failed: %w", name, ExportMeta, err)
		}
		if err := json.Unmarshal(raw, &p.manifest); err != nil {
			_ = p.Close(ctx)
			return nil, fmt.Errorf("wasm plugin %s: %s: bad manifest json: %w", name, ExportMeta, err)
		}
	}
	if err := p.validate(); err != nil {
		_ = p.Close(ctx)
		return nil, err
	}
	// 裸 wasm 的清单(自定义段或 egop_meta)此刻已知:按 egop.pool 补池,
	// 与 zip 形态同语义(装载期实例化只建了 1 个)。
	if err := p.growPool(ctx, p.poolSize()); err != nil {
		_ = p.Close(ctx)
		return nil, err
	}
	return p, nil
}

// instantiate 建立 runtime(含 WASI 无 fs、宿主注入函数表)、编译 guest(compiled
// 保留作池化/revive 锚)并按池大小实例化。
func (p *Plugin) instantiate(ctx context.Context, wasmBytes []byte, opts Options, pool int) error {
	// revive 锚:留住代码字节与装载选项(意外打断后按原样重建;显式 Close 不适用)。
	p.wasmRaw = append([]byte(nil), wasmBytes...)
	p.loadOpts = opts
	lim := p.memoryPages(opts)
	p.memPages = lim
	p.logFn = opts.LogFn
	rcfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(lim).WithCloseOnContextDone(true)
	if cc := sharedCompilationCache(); cc != nil {
		rcfg = rcfg.WithCompilationCache(cc)
	}
	r := wazero.NewRuntimeWithConfig(ctx, rcfg)
	p.runtime = r

	// 失败路径统一回收:runtime(goroutine/mmapped 内存)与可能已编译的模块在返回
	// error 时一并关闭,避免每个坏包在 autoload/mount 轮询里泄漏一个 runtime。
	success := false
	defer func() {
		if success {
			return
		}
		if p.compiled != nil {
			_ = p.compiled.Close(ctx)
			p.compiled = nil
		}
		_ = r.Close(ctx)
	}()

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return fmt.Errorf("wasm plugin %s: install wasi: %w", p.name, err)
	}
	if err := p.buildHostModule(ctx, r); err != nil {
		return fmt.Errorf("wasm plugin %s: host module: %w", p.name, err)
	}
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("wasm plugin %s: compile: %w", p.name, err)
	}
	p.compiled = compiled
	if err := validateHostImports(p.name, compiled.ImportedFunctions()); err != nil {
		return err
	}
	if pool < 1 {
		pool = 1
	}
	if pool > 1 {
		// 有效池大小必须可见:容量类故障(实例被占满后静默排队)在外部只表现为
		// 「agent 不动」,没有这行就无法区分「池小」与「模型/网络慢」。
		fmt.Fprintf(os.Stderr, "%s wasm plugin %s: pool=%d(单实例上限 %dMiB,主机内存 %dMiB)\n",
			time.Now().Format("2006/01/02 15:04:05"), p.name, pool,
			p.instMaxBytes()/(1<<20), hostMemoryBytes()/(1<<20))
	}
	for idx := 0; idx < pool; idx++ {
		i := newInst(fmt.Sprintf("%s#%d", p.name, idx))
		mod, err := p.instantiateOne(ctx, compiled, i.name)
		if err != nil {
			return err
		}
		i.mod = mod
		p.insts = append(p.insts, i)
		p.byName[i.name] = i
	}
	success = true
	return nil
}

// instantiateOne 从保留的 compiled 再实例化一个 module(池增补/revive 共用)。
// Go wasip1 插件用 reactor 模式(-buildmode=c-shared)导出 _initialize、command 模式
// 导出 _start,WAT 手写插件可能两者皆无。wazero 对列表里不存在的 start 函数是宽容
// 跳过的,故直接按"reactor 优先、command 兜底"列出——保证 Go 插件的 init/运行时在
// egop_meta 之前被初始化(否则 Go 插件拿不到注册实例)。
// 系统时钟三件必接:wazero ModuleConfig 缺省 Walltime/Nanotime 是不可用桩
// (纪元 2022 假钟/ENOSYS),guest 内 time.Now/Since/Sleep 全部拿到假时间或
// 报错——TTL/排程/退避类插件逻辑静默错乱(testdata/clock.wat 夹具固化,防回退)。
func (p *Plugin) instantiateOne(ctx context.Context, compiled wazero.CompiledModule, modName string) (api.Module, error) {
	cfg := wazero.NewModuleConfig().WithName(modName).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().
		WithStartFunctions("_initialize", "_start")
	mod, err := p.runtime.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: instantiate %s: %w", p.name, modName, err)
	}
	return mod, nil
}

// validate 校验 ABI 结构性前提(清单之外的部分)。实例化后的正常装载与
// InspectPackage 的纯编译检查共用 validateStructuralABI,防两条入口逐渐漂移。
func (p *Plugin) validate() error {
	m := p.insts[0].mod
	return validateStructuralABI(p.name, p.manifest, m.ExportedFunctionDefinitions(), m.ExportedMemoryDefinitions())
}

// validateHostImports 是正常装载与静态检查共用的宿主 import 白名单。
func validateHostImports(pluginName string, imports []api.FunctionDefinition) error {
	for _, def := range imports {
		modName, fName, _ := def.Import()
		if modName != HostModuleName && modName != WASIModule {
			return fmt.Errorf("wasm plugin %s: import %q.%q not allowed (only %q and %q)",
				pluginName, modName, fName, HostModuleName, WASIModule)
		}
		if modName == HostModuleName {
			var expected *hostOp
			for i := range hostOps {
				if hostOps[i].name == fName {
					expected = &hostOps[i]
					break
				}
			}
			if expected == nil {
				return fmt.Errorf("wasm plugin %s: unknown host import %q.%q", pluginName, modName, fName)
			}
			params := def.ParamTypes()
			if len(params) != expected.pairs*2 || len(def.ResultTypes()) != 1 || def.ResultTypes()[0] != api.ValueTypeI64 {
				return fmt.Errorf("wasm plugin %s: host import %q.%q has incompatible signature", pluginName, modName, fName)
			}
			for _, typ := range params {
				if typ != api.ValueTypeI32 {
					return fmt.Errorf("wasm plugin %s: host import %q.%q has incompatible signature", pluginName, modName, fName)
				}
			}
		}
	}
	return nil
}

// validateStructuralABI 只读 wazero 的模块定义,不调用任何 guest 函数。
// call/on_hook 的 4 参旧形与 6 参 Origin 新形都保留;tool 始终是三对 ptr/len。
func validateStructuralABI(pluginName string, mf contract.Manifest, functions map[string]api.FunctionDefinition, memories map[string]api.MemoryDefinition) error {
	if mf.ID == "" {
		return fmt.Errorf("wasm plugin %s: manifest id required", pluginName)
	}
	if memories["memory"] == nil {
		return fmt.Errorf("wasm plugin %s: memory not exported (ABI requires (memory (export \"memory\")))", pluginName)
	}
	alloc := functions[ExportHostAlloc]
	if alloc == nil {
		return fmt.Errorf("wasm plugin %s: export %q missing (ABI required)", pluginName, ExportHostAlloc)
	}
	if err := validateExportSignature(pluginName, ExportHostAlloc, alloc, []int{1}, api.ValueTypeI32, "1 i32 param and 1 i32 result"); err != nil {
		return err
	}
	if len(mf.Provides.Functions) > 0 && functions[ExportCall] == nil {
		return fmt.Errorf("wasm plugin %s: declares functions but export %q missing", pluginName, ExportCall)
	}
	if len(mf.Tools) > 0 && functions[ExportTool] == nil {
		return fmt.Errorf("wasm plugin %s: declares tools but export %q missing", pluginName, ExportTool)
	}
	for _, name := range []string{ExportCall, ExportOnHook} {
		if fn := functions[name]; fn != nil {
			if err := validateExportSignature(pluginName, name, fn, []int{4, 6}, api.ValueTypeI64, "4 or 6 i32 params and 1 i64 result"); err != nil {
				return err
			}
		}
	}
	if fn := functions[ExportTool]; fn != nil {
		if err := validateExportSignature(pluginName, ExportTool, fn, []int{6}, api.ValueTypeI64, "6 i32 params and 1 i64 result"); err != nil {
			return err
		}
	}
	return nil
}

func validateExportSignature(pluginName, name string, fn api.FunctionDefinition, paramCounts []int, result api.ValueType, want string) error {
	params := fn.ParamTypes()
	countOK := false
	for _, count := range paramCounts {
		if len(params) == count {
			countOK = true
			break
		}
	}
	results := fn.ResultTypes()
	if countOK && len(results) == 1 && results[0] == result {
		valid := true
		for _, typ := range params {
			if typ != api.ValueTypeI32 {
				valid = false
				break
			}
		}
		if valid {
			return nil
		}
	}
	return fmt.Errorf("wasm plugin %s: export %q has params %v results %v (ABI wants %s)", pluginName, name, params, results, want)
}

// validateCodeless 校验无代码插件(纯清单/资产):只需 id,且不得声明任何需要
// guest 代码兑现的面——函数/工具/配置/自有 hook 点(无代码即无法兑现,声明了
// 就是假契约,fail-closed 拒载;清单扩展/资产/依赖声明等纯元数据面不受限)。
func (p *Plugin) validateCodeless() error {
	if p.manifest.ID == "" {
		return fmt.Errorf("wasm plugin %s: manifest id required", p.name)
	}
	switch {
	case len(p.manifest.Provides.Functions) > 0:
		return fmt.Errorf("wasm plugin %s: codeless bundle must not declare functions", p.name)
	case len(p.manifest.Tools) > 0:
		return fmt.Errorf("wasm plugin %s: codeless bundle must not declare tools", p.name)
	case len(p.manifest.Provides.Config) > 0:
		return fmt.Errorf("wasm plugin %s: codeless bundle must not declare config fields", p.name)
	case len(p.manifest.Provides.Hooks) > 0:
		return fmt.Errorf("wasm plugin %s: codeless bundle must not declare hook points", p.name)
	}
	return nil
}

// parseCustomManifest 从 wasm 二进制自定义段(名 egop.manifest)取内嵌清单。
// 首个命中即返;无该段 → found=false;段内 JSON 坏 → error。
func parseCustomManifest(bin []byte) (contract.Manifest, bool, error) {
	var mf contract.Manifest
	if len(bin) < 8 {
		return mf, false, nil
	}
	if string(bin[:4]) != "\x00asm" {
		return mf, false, fmt.Errorf("not a wasm binary (bad magic)")
	}
	pos := 8
	for pos < len(bin) {
		id := bin[pos]
		pos++
		size, n := readULEB(bin, pos)
		if n == 0 {
			return mf, false, fmt.Errorf("truncated wasm: bad section size")
		}
		pos += n
		end := pos + size
		if end > len(bin) {
			return mf, false, fmt.Errorf("truncated wasm: section overruns file")
		}
		payload := bin[pos:end]
		if id == 0 {
			nlen, nn := readULEB(payload, 0)
			if nn == 0 || nlen > len(payload)-nn {
				return mf, false, fmt.Errorf("truncated wasm: bad custom section name")
			}
			if string(payload[nn:nn+nlen]) == ManifestSection {
				raw := payload[nn+nlen:]
				if err := json.Unmarshal(raw, &mf); err != nil {
					return mf, false, fmt.Errorf("bad %s section json: %w", ManifestSection, err)
				}
				return mf, true, nil
			}
		}
		pos = end
	}
	return mf, false, nil
}

// readULEB 读无符号 LEB128(大小上限约 2^35,防恶意超长)。
func readULEB(bin []byte, pos int) (int, int) {
	var val int
	shift := 0
	for i := pos; i < len(bin); i++ {
		b := bin[i]
		val |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return val, i - pos + 1
		}
		if shift > 35 {
			return 0, 0
		}
	}
	return 0, 0
}

// zipPackage 是拆包结果及观察元数据;正常装载只取前三个字段,静态检查保留
// 条目数与实际解压量供上传方展示。
type zipPackage struct {
	manifest   contract.Manifest
	wasm       []byte
	assets     map[string][]byte
	entryCount int
	totalBytes int64
}

// parseZip 拆 zip 包(*.egop.zip 或注入后缀):manifest.json(必需)+
// plugin.wasm(可选)+ assets/*(可选)。plugin.wasm 缺省 = 「无代码插件」
// (纯清单/资产,如 UI 插件):manifest 声明扩展、assets 携带静态资源,宿主经
// Meta()/Assets() 消费,不执行任何 guest 代码。
// 插件是不可信输入:每条目与聚合解压量都设上限(头部声明值预检 + 读取时硬限,
// 谎报尺寸的炸弹包在触顶即拒载),防 zip bomb 打爆宿主内存。
func parseZip(data []byte, opts Options) (contract.Manifest, []byte, map[string][]byte, error) {
	pkg, err := parseZipPackage(data, opts)
	if err != nil {
		return contract.Manifest{}, nil, nil, err
	}
	return pkg.manifest, pkg.wasm, pkg.assets, nil
}

func parseZipPackage(data []byte, opts Options) (zipPackage, error) {
	var pkg zipPackage
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return pkg, fmt.Errorf("bad zip: %w", err)
	}
	pkg.entryCount = len(zr.File)
	entryLim := opts.MaxEntryBytes
	if entryLim <= 0 {
		entryLim = DefaultMaxEntryBytes
	}
	totalLim := opts.MaxTotalBytes
	if totalLim <= 0 {
		totalLim = DefaultMaxTotalBytes
	}
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if len(zr.File) > maxEntries {
		return pkg, fmt.Errorf("zip: %d entries exceeds limit %d", len(zr.File), maxEntries)
	}
	var manRaw []byte
	pkg.assets = map[string][]byte{}
	seen := map[string]string{}
	var declaredTotal, total int64
	for _, f := range zr.File {
		if err := validateZipEntry(f); err != nil {
			return pkg, err
		}
		key := strings.ToLower(strings.TrimSuffix(f.Name, "/"))
		if previous, ok := seen[key]; ok {
			return pkg, fmt.Errorf("zip entry %s: duplicate or case-colliding path (also %s)", f.Name, previous)
		}
		seen[key] = f.Name
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > uint64(entryLim) {
			return pkg, fmt.Errorf("zip entry %s: declared size %d exceeds per-entry limit %d", f.Name, f.UncompressedSize64, entryLim)
		}
		declared := int64(f.UncompressedSize64)
		if declaredTotal > totalLim-declared {
			return pkg, fmt.Errorf("zip: aggregate uncompressed size exceeds limit %d", totalLim)
		}
		declaredTotal += declared
		rc, err := f.Open()
		if err != nil {
			return pkg, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		// 硬限读取同时受单条与剩余聚合预算约束;+1 只用于探测超限,并避开
		// MaxInt64 溢出。头部声明偏小时也不会先读出超过总预算的一整条。
		readLim := entryLim
		remaining := totalLim - total
		if remaining < readLim {
			readLim = remaining
		}
		if readLim < int64(1<<63-1) {
			readLim++
		}
		raw, err := io.ReadAll(io.LimitReader(rc, readLim))
		rc.Close()
		if err != nil {
			return pkg, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		if int64(len(raw)) > entryLim {
			return pkg, fmt.Errorf("zip entry %s: exceeds per-entry limit %d", f.Name, entryLim)
		}
		if int64(len(raw)) > remaining {
			return pkg, fmt.Errorf("zip: aggregate uncompressed size exceeds limit %d", totalLim)
		}
		total += int64(len(raw))
		switch {
		case f.Name == "manifest.json":
			manRaw = raw
		case f.Name == "plugin.wasm":
			pkg.wasm = raw
		case strings.HasPrefix(f.Name, "assets/"):
			key := strings.TrimPrefix(f.Name, "assets/")
			// 资产名会经 Assets()/read_asset 交给消费方(可能落盘服务),拒绝
			// 绝对路径与 ".." 穿越(zip-slip 防在源头)。
			if key == "" || path.Clean("/"+key) != "/"+key {
				return pkg, fmt.Errorf("zip entry %s: unsafe asset name", f.Name)
			}
			pkg.assets[key] = raw
		}
	}
	pkg.totalBytes = total
	if manRaw == nil {
		return pkg, fmt.Errorf("%s zip missing manifest.json (required bundle layout)", SuffixZip)
	}
	if err := json.Unmarshal(manRaw, &pkg.manifest); err != nil {
		return pkg, fmt.Errorf("bad manifest.json: %w", err)
	}
	return pkg, nil
}

// validateZipEntry 在读取任何条目之前检查全包路径与文件类型。未知的安全普通
// 文件仍可为兼容而忽略,但恶意条目不能因为不在 assets/ 下就被静默跳过。
func validateZipEntry(f *zip.File) error {
	name := f.Name
	unsafe := func(reason string) error {
		return fmt.Errorf("zip entry %q: %s", name, reason)
	}
	if name == "" {
		return unsafe("empty path")
	}
	if strings.ContainsRune(name, '\x00') {
		return unsafe("NUL in path")
	}
	if strings.Contains(name, `\`) {
		return unsafe("backslash in path")
	}
	if path.IsAbs(name) || hasWindowsDrive(name) {
		return unsafe("absolute path")
	}
	cleanName := strings.TrimSuffix(name, "/")
	if cleanName == "" || cleanName == "." {
		return unsafe("empty or non-canonical path")
	}
	for _, part := range strings.Split(cleanName, "/") {
		if part == ".." {
			return unsafe("parent path traversal")
		}
	}
	if path.Clean(cleanName) != cleanName {
		return unsafe("non-canonical path")
	}
	mode := f.Mode()
	if mode&fs.ModeSymlink != 0 {
		return unsafe("symlink not allowed")
	}
	if f.FileInfo().IsDir() {
		if typ := mode & fs.ModeType; typ != 0 && typ != fs.ModeDir {
			return unsafe("special file not allowed")
		}
		return nil
	}
	if !mode.IsRegular() {
		return unsafe("special file not allowed")
	}
	return nil
}

func hasWindowsDrive(name string) bool {
	if len(name) < 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ScanFS 遍历只读文件系统发现插件包(后缀判定与 LoadFS 同一 helper:内置
// *.egop.wasm / *.egop.zip + Options.ExtraSuffixes;隐藏目录跳过),用
// fs.ReadFile 读字节再 LoadFS——不触碰 os/filepath,浏览器/内嵌可注入
// fstest.MapFS 或自定义 FS。单包失败不阻断其余:返回 (成功插件, 逐包错误)。
func ScanFS(ctx context.Context, fsys fs.FS, opts Options) ([]*Plugin, []error) {
	var plugs []*Plugin
	var errs []error
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !IsPluginFile(d.Name(), opts.ExtraSuffixes) {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		p, err := LoadFS(ctx, data, d.Name(), opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		plugs = append(plugs, p)
		return nil
	})
	return plugs, errs
}

// ScanDir 递归扫描 OS 目录(ScanFS 的 os.DirFS 便捷面)。
func ScanDir(ctx context.Context, dir string, opts Options) ([]*Plugin, []error) {
	return ScanFS(ctx, os.DirFS(dir), opts)
}

// ParseDirs 解析插件目录设置值:JSON 字符串数组或逗号分隔串。
// 空/缺省 → nil(由装配层回落默认目录)。
func ParseDirs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return cleanDirs(arr)
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return cleanDirs(strings.Split(s, ","))
}

func cleanDirs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}
