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
}

// LoadFile 按路径加载插件包(name 取文件名以判定包形态)。
func LoadFile(ctx context.Context, path string, opts Options) (*Plugin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadFS(ctx, data, filepath.Base(path), opts)
}

// LoadFS 加载插件包字节(形态由文件名后缀判定:*.egop.zip 或 *.egop.wasm)。
func LoadFS(ctx context.Context, data []byte, name string, opts Options) (*Plugin, error) {
	if name == "" {
		name = "plugin"
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, SuffixZip) {
		mf, wasmBytes, assets, err := parseZip(data)
		if err != nil {
			return nil, fmt.Errorf("wasm plugin %s: %w", name, err)
		}
		p := newPlugin(name)
		p.manifest = mf
		p.assets = assets
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
		return nil, fmt.Errorf("wasm plugin %s: unsupported suffix (want %s or %s)", name, SuffixWasm, SuffixZip)
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

// parseZip 拆 .egop.zip:manifest.json(必需)+ plugin.wasm(必需)+ assets/*(可选)。
func parseZip(data []byte) (contract.Manifest, []byte, map[string][]byte, error) {
	var mf contract.Manifest
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return mf, nil, nil, fmt.Errorf("bad zip: %w", err)
	}
	var manRaw, wasmBytes []byte
	assets := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return mf, nil, nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return mf, nil, nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		switch {
		case f.Name == "manifest.json":
			manRaw = raw
		case f.Name == "plugin.wasm":
			wasmBytes = raw
		case strings.HasPrefix(f.Name, "assets/"):
			assets[strings.TrimPrefix(f.Name, "assets/")] = raw
		}
	}
	if manRaw == nil {
		return mf, nil, nil, fmt.Errorf("%s zip missing manifest.json (required bundle layout)", SuffixZip)
	}
	if wasmBytes == nil {
		return mf, nil, nil, fmt.Errorf("%s zip missing plugin.wasm", SuffixZip)
	}
	if err := json.Unmarshal(manRaw, &mf); err != nil {
		return mf, nil, nil, fmt.Errorf("bad manifest.json: %w", err)
	}
	return mf, wasmBytes, assets, nil
}

// ScanFS 遍历只读文件系统发现 *.egop.wasm / *.egop.zip(隐藏目录跳过),用
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
		lower := strings.ToLower(d.Name())
		if !strings.HasSuffix(lower, SuffixWasm) && !strings.HasSuffix(lower, SuffixZip) {
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
