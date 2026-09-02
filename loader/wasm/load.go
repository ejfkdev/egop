// 插件包加载器:*.egop.wasm(自定义段 egop.manifest 内置清单,缺省回退 egop_meta
// 导出)与 *.egop.zip(manifest.json + plugin.wasm + assets/)两种形态,加目录
// 递归发现 ScanDir 与设置键解析 ParseDirs。
package wasm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ejfkdev/egop/contract"
	"github.com/tetratelabs/wazero"
	wasi_snapshot_preview1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// DefaultMaxPages 是 guest 内存上限的默认值(页,每页 64KiB → 64MiB)。
const DefaultMaxPages uint32 = 1024

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
}

// zip 解压默认上限:单条目 256MiB、整包聚合 1GiB(正常插件包远小于此;
// 恶意高压缩比包在触顶时立即拒载,不被读进内存)。
const (
	DefaultMaxEntryBytes int64 = 256 << 20
	DefaultMaxTotalBytes int64 = 1 << 30
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
		if err := p.instantiate(ctx, wasmBytes, opts); err != nil {
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
	if err := p.instantiate(ctx, data, opts); err != nil {
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
	return p, nil
}

// instantiate 建立 runtime(含 WASI 无 fs、宿主注入函数表)并编译/实例化 guest。
func (p *Plugin) instantiate(ctx context.Context, wasmBytes []byte, opts Options) error {
	lim := opts.MaxMemoryPages
	if lim == 0 {
		lim = DefaultMaxPages
	}
	p.logFn = opts.LogFn
	rcfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(lim)
	r := wazero.NewRuntimeWithConfig(ctx, rcfg)
	p.runtime = r

	// 失败路径统一回收:runtime(goroutine/mmapped 内存)与可能已编译的模块在返回
	// error 时一并关闭,避免每个坏包在 autoload/mount 轮询里泄漏一个 runtime。
	var compiled wazero.CompiledModule
	success := false
	defer func() {
		if success {
			return
		}
		if compiled != nil {
			_ = compiled.Close(ctx)
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
	for _, def := range compiled.ImportedFunctions() {
		modName, fName, _ := def.Import()
		if modName != HostModuleName && modName != WASIModule {
			return fmt.Errorf("wasm plugin %s: import %q.%q not allowed (only %q and %q)",
				p.name, modName, fName, HostModuleName, WASIModule)
		}
		if modName == HostModuleName && !hostImportSet[fName] {
			return fmt.Errorf("wasm plugin %s: unknown host import %q.%q", p.name, modName, fName)
		}
	}
	// Go wasip1 插件用 reactor 模式(-buildmode=c-shared)导出 _initialize、command 模式
	// 导出 _start,WAT 手写插件可能两者皆无。wazero 对列表里不存在的 start 函数是宽容
	// 跳过的,故直接按"reactor 优先、command 兜底"列出——保证 Go 插件的 init/运行时在
	// egop_meta 之前被初始化(否则 Go 插件拿不到注册实例)。
	cfg := wazero.NewModuleConfig().WithName(p.name).WithStartFunctions("_initialize", "_start")
	mod, err := r.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return fmt.Errorf("wasm plugin %s: instantiate: %w", p.name, err)
	}
	_ = compiled.Close(ctx)
	p.mod = mod
	success = true
	return nil
}

// validate 校验 ABI 结构性前提(清单之外的部分)。
func (p *Plugin) validate() error {
	if p.manifest.ID == "" {
		return fmt.Errorf("wasm plugin %s: manifest id required", p.name)
	}
	if p.mod.Memory() == nil {
		return fmt.Errorf("wasm plugin %s: memory not exported (ABI requires (memory (export \"memory\")))", p.name)
	}
	if p.mod.ExportedFunction(ExportHostAlloc) == nil {
		return fmt.Errorf("wasm plugin %s: export %q missing (ABI required)", p.name, ExportHostAlloc)
	}
	if len(p.manifest.Provides.Functions) > 0 && p.mod.ExportedFunction(ExportCall) == nil {
		return fmt.Errorf("wasm plugin %s: declares functions but export %q missing", p.name, ExportCall)
	}
	if len(p.manifest.Tools) > 0 && p.mod.ExportedFunction(ExportTool) == nil {
		return fmt.Errorf("wasm plugin %s: declares tools but export %q missing", p.name, ExportTool)
	}
	// 双形状导出按精确元数校验(4=旧版两参对 / 6=新版含 Origin 第三参对;
	// 调用侧按 ==6 决定是否传 Origin,其它元数无法兑现任何一种 ABI,装载期即拒)。
	for _, name := range []string{ExportCall, ExportOnHook} {
		if fn := p.mod.ExportedFunction(name); fn != nil {
			if n := len(fn.Definition().ParamTypes()); n != 4 && n != 6 {
				return fmt.Errorf("wasm plugin %s: export %q has %d params (ABI wants 4 or 6 i32)", p.name, name, n)
			}
		}
	}
	return nil
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

// parseZip 拆 zip 包(*.egop.zip 或注入后缀):manifest.json(必需)+
// plugin.wasm(可选)+ assets/*(可选)。plugin.wasm 缺省 = 「无代码插件」
// (纯清单/资产,如 UI 插件):manifest 声明扩展、assets 携带静态资源,宿主经
// Meta()/Assets() 消费,不执行任何 guest 代码。
// 插件是不可信输入:每条目与聚合解压量都设上限(头部声明值预检 + 读取时硬限,
// 谎报尺寸的炸弹包在触顶即拒载),防 zip bomb 打爆宿主内存。
func parseZip(data []byte, opts Options) (contract.Manifest, []byte, map[string][]byte, error) {
	var mf contract.Manifest
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return mf, nil, nil, fmt.Errorf("bad zip: %w", err)
	}
	entryLim := opts.MaxEntryBytes
	if entryLim <= 0 {
		entryLim = DefaultMaxEntryBytes
	}
	totalLim := opts.MaxTotalBytes
	if totalLim <= 0 {
		totalLim = DefaultMaxTotalBytes
	}
	var manRaw, wasmBytes []byte
	assets := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if int64(f.UncompressedSize64) > entryLim {
			return mf, nil, nil, fmt.Errorf("zip entry %s: declared size %d exceeds per-entry limit %d", f.Name, f.UncompressedSize64, entryLim)
		}
		total += int64(f.UncompressedSize64)
		if total > totalLim {
			return mf, nil, nil, fmt.Errorf("zip: aggregate uncompressed size exceeds limit %d", totalLim)
		}
		rc, err := f.Open()
		if err != nil {
			return mf, nil, nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		// 硬限读取:头部声明可以是谎报,以实际字节数为准(+1 探测超限)。
		raw, err := io.ReadAll(io.LimitReader(rc, entryLim+1))
		rc.Close()
		if err != nil {
			return mf, nil, nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		if int64(len(raw)) > entryLim {
			return mf, nil, nil, fmt.Errorf("zip entry %s: exceeds per-entry limit %d", f.Name, entryLim)
		}
		switch {
		case f.Name == "manifest.json":
			manRaw = raw
		case f.Name == "plugin.wasm":
			wasmBytes = raw
		case strings.HasPrefix(f.Name, "assets/"):
			key := strings.TrimPrefix(f.Name, "assets/")
			// 资产名会经 Assets()/read_asset 交给消费方(可能落盘服务),拒绝
			// 绝对路径与 ".." 穿越(zip-slip 防在源头)。
			if key == "" || path.Clean("/"+key) != "/"+key {
				return mf, nil, nil, fmt.Errorf("zip entry %s: unsafe asset name", f.Name)
			}
			assets[key] = raw
		}
	}
	if manRaw == nil {
		return mf, nil, nil, fmt.Errorf("%s zip missing manifest.json (required bundle layout)", SuffixZip)
	}
	if err := json.Unmarshal(manRaw, &mf); err != nil {
		return mf, nil, nil, fmt.Errorf("bad manifest.json: %w", err)
	}
	return mf, wasmBytes, assets, nil
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
