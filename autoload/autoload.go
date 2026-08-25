// Package autoload 是 egop 的**热更目录装载器**(cordis loader+hmr 的
// 对应物):轮询插件目录,把 `*.egop.wasm` / `*.egop.zip` 文件的增/改/删映射为
// 宿主上的注册/热替换/卸载。安全语义(对标 cordis 的失败隔离与 LIFO 清退):
//
//   - 内容 hash 判变(sha256),两段确认(连续两轮一致才应用)天然抗半截写入;
//   - 替换失败 = **回退保旧版**(新包关闭丢弃,旧版继续服务),仅事件告警;
//   - 替换成功后**重放已下发配置**(AppliedConfig → SetConfig),行为账连续;
//   - 跨目录/重复 id = 先到先得,后来者事件告警,不阻断;
//   - 被依赖插件被删 → Remove fail-closed(宿主裁决),事件告警,不影响其它。
package autoload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ejfkdev/egop/loader"
	"github.com/ejfkdev/egop/loader/wasm"
)

// Action 是热更事件的动词。
type Action string

const (
	ActionRegister Action = "register" // 新包进册
	ActionReplace  Action = "replace"  // 同 id 热替换
	ActionRemove   Action = "remove"   // 包删除/卸载
	ActionFailed   Action = "failed"   // 装载/替换/卸载失败(回退旧版或保留现状)
)

// Event 是一起热更事件的观测记录(失败类动作 Err 非空)。
type Event struct {
	Action   Action `json:"action"`
	PluginID string `json:"plugin_id,omitempty"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Err      error  `json:"error,omitempty"`
}

func (e Event) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s %s %s: %v", e.Action, e.PluginID, e.Path, e.Err)
	}
	return fmt.Sprintf("%s %s %s", e.Action, e.PluginID, e.Path)
}

// Options 是监视器的可选项(零值可用)。
type Options struct {
	Interval time.Duration                 // 轮询周期;0 = 1s
	Logf     func(format string, a ...any) // 生命周期日志;nil = 静默
	FS       fs.FS                         // 注入的只读文件系统(内嵌/浏览器用);nil = 操作系统目录
}

// dot 是一个目录下的一行文件观察。
type unit struct {
	path    string // 规格化路径(相对原样,供事件展示)
	hash    string // 当前已加载内容 hash
	pending string // 上轮观察到的候选 hash(两段确认)
	id      string // 在册插件 id(manifest)
	plugin  *wasm.Plugin
	loaded  bool
	read    func() ([]byte, error) // 读取当前内容(os 目录或注入 FS)
}

// Watcher 监视一组插件目录并把变更应用到宿主。
type Watcher struct {
	hf    loader.HostFace
	dirs  []string
	opts  Options
	fsys  fs.FS // 注入的只读文件系统;nil = 操作系统目录
	mu    sync.Mutex
	units map[string]*unit // path → 观察单元(含已加载与 pending)
	evs   chan Event
	stop  chan struct{}
	// startOnce/stopOnce 分开持有:若共用同一 sync.Once,Start 会先 consume 掉它,
	// 使之后的 Stop 变成 no-op(热更 goroutine 永不退出、Close 后仍轮询)。
	startOnce sync.Once
	stopOnce  sync.Once
}

// New 构造监视器(未启动;Poll 可显式驱动,Start 开轮询循环)。
// evs 事件流容量为 64(不等消费者时丢弃新事件,不阻塞装载)。
func New(hf loader.HostFace, dirs []string, opts Options) *Watcher {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	return &Watcher{
		hf:    hf,
		dirs:  append([]string(nil), dirs...),
		opts:  opts,
		fsys:  opts.FS,
		units: map[string]*unit{},
		evs:   make(chan Event, 64),
		stop:  make(chan struct{}),
	}
}

// Events 返回事件观测流(无论 Start 还是手动 Poll 都会投递)。
func (w *Watcher) Events() <-chan Event { return w.evs }

func (w *Watcher) logf(format string, a ...any) {
	if w.opts.Logf != nil {
		w.opts.Logf(format, a...)
	}
}

// emit 投递事件(缓冲满即丢,不阻塞装载链路)。
func (w *Watcher) emit(e Event) {
	select {
	case w.evs <- e:
	default:
	}
}

// Start 开轮询循环(幂等;Stop 停)。
func (w *Watcher) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		go func() {
			t := time.NewTicker(w.opts.Interval)
			defer t.Stop()
			for {
				select {
				case <-w.stop:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					w.pollOnce(ctx)
				}
			}
		}()
	})
}

// Stop 停止轮询(不卸载已注册插件;清退用 Watcher 外的宿主总闸)。
func (w *Watcher) Stop() { w.stopOnce.Do(func() { close(w.stop) }) }

// Poll 显式驱动一轮(装配层可与 Start 混用;并发调用互斥)。
func (w *Watcher) Poll(ctx context.Context) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pollOnceLocked(ctx)
}

func (w *Watcher) pollOnce(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pollOnceLocked(ctx)
}

func (w *Watcher) pollOnceLocked(ctx context.Context) []Event {
	var events []Event
	seen := map[string]string{}                 // path → 本轮内容 hash(只收纳法后缀)
	read := map[string]func() ([]byte, error){} // path → 读取当前内容的闭包
	for _, dir := range w.dirs {
		fsys, root := w.sourceFor(dir)
		_ = fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			lower := strings.ToLower(d.Name())
			if !strings.HasSuffix(lower, wasm.SuffixWasm) && !strings.HasSuffix(lower, wasm.SuffixZip) {
				return nil
			}
			h, err := hashFS(fsys, path)
			if err != nil {
				return nil // 半截写等时时读失败:下一轮再试
			}
			wpath := w.keyFor(dir, root, path)
			seen[wpath] = h
			read[wpath] = func() ([]byte, error) { return fs.ReadFile(fsys, path) }
			return nil
		})
	}
	// 1. 删除:本轮未见且已加载 → 立即卸载(宿主裁决 fail-closed)
	for p, u := range w.units {
		if !u.loaded {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		ev := Event{Action: ActionRemove, PluginID: u.id, Path: p}
		if _, err := w.hf.Remove(u.id, false); err != nil {
			ev.Err = err // 被依赖:保留在册(插件继续服务),仅告警
		} else {
			_ = u.plugin.Close(ctx)
		}
		delete(w.units, p)
		events = append(events, ev)
	}
	// 2. 新增/变更:两段确认后装载
	for p, h := range seen {
		u, ok := w.units[p]
		if !ok {
			u = &unit{path: p, read: read[p]}
			w.units[p] = u
		}
		if u.loaded && h == u.hash {
			continue // 无变化
		}
		if u.pending != h {
			u.pending = h // 首轮观察:确认为稳定内容
			continue
		}
		// 第二轮一致:装载应用(change 或 add 同路径)
		ev := w.applyChange(ctx, u, h, p, true)
		events = append(events, ev)
	}
	return events
}

// applyChange 装载新内容和应用(入册/替换);回退语义见包注释。
func (w *Watcher) applyChange(ctx context.Context, u *unit, h, p string, _ bool) Event {
	data, err := u.read()
	if err != nil {
		u.pending = ""
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err}
	}
	np, err := wasm.LoadFS(ctx, data, filepath.Base(p), wasm.Options{})
	if err != nil {
		if !u.loaded {
			delete(w.units, p) // 从未入册的坏包:不留观察槽,待下次变化再试
		} else {
			u.pending = "" // 旧版在册:清候选,等待真变化
		}
		// 装载失败一律 ActionFailed(含"旧版在册的坏替换"),区别于真正替换成功的 ActionReplace。
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err}
	}
	meta := np.Meta()
	if !u.loaded {
		if err := w.hf.Register(np); err != nil {
			_ = np.Close(ctx)
			delete(w.units, p)
			return Event{Action: ActionFailed, PluginID: meta.ID, Path: p, Err: err}
		}
		u.id, u.hash, u.loaded, u.plugin = meta.ID, h, true, np
		w.logf("autoload: plugin %s registered (%s)", meta.ID, p)
		return Event{Action: ActionRegister, PluginID: meta.ID, Path: p, Version: meta.Version}
	}
	// 替换:同 id 走宿主 Replace;id 变了 = 旧卸载 + 新注册
	if meta.ID != u.id {
		ev := Event{Action: ActionReplace, PluginID: u.id, Path: p, Version: meta.Version}
		if err := w.hf.Register(np); err != nil {
			_ = np.Close(ctx)
			u.pending = ""
			ev.Action = ActionFailed
			ev.Err = fmt.Errorf("new manifest id %q: %w", meta.ID, err)
			return ev
		}
		oldID := u.id
		if _, err := w.hf.Remove(u.id, false); err != nil {
			ev.Err = err // 旧 id 有依赖者:新旧并存,旧版继续服务
		} else {
			_ = u.plugin.Close(ctx)
		}
		u.id, u.hash, u.plugin = meta.ID, h, np
		w.logf("autoload: plugin %s re-identified (was %s)", meta.ID, oldID)
		return ev
	}
	old := u.plugin
	prevCfg, hadCfg := w.hf.AppliedConfig(u.id)
	if err := w.hf.Replace(np); err != nil {
		_ = np.Close(ctx)
		u.pending = ""
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: fmt.Errorf("replace refused, old version kept: %w", err)}
	}
	_ = old.Close(ctx)
	u.hash, u.plugin = h, np
	var replayErr error
	if hadCfg {
		if err := w.hf.SetConfig(u.id, prevCfg); err != nil {
			replayErr = fmt.Errorf("config replay: %w", err)
		}
	}
	w.logf("autoload: plugin %s replaced (%s %s)", u.id, p, meta.Version)
	return Event{Action: ActionReplace, PluginID: u.id, Path: p, Version: meta.Version, Err: replayErr}
}

// sourceFor 解析一个 dir 为 (遍历/读取用 FS, 遍历根)。注入 FS 时 dir 是其中的
// 根目录;nil FS 时把 dir 包成 os.DirFS,根为 "."。
func (w *Watcher) sourceFor(dir string) (fs.FS, string) {
	if w.fsys != nil {
		return w.fsys, dir
	}
	return os.DirFS(dir), "."
}

// keyFor 把遍历路径映射回稳定、唯一的展示/建档键(OS 模式回到原 dir 前缀)。
func (w *Watcher) keyFor(dir, root, path string) string {
	rel := strings.TrimPrefix(path, root)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return dir
	}
	return filepath.Join(dir, rel)
}

// hashFS 读 fs.FS 内路径并取内容 sha256。
func hashFS(fsys fs.FS, path string) (string, error) {
	b, err := fs.ReadFile(fsys, path)
	if err != nil {
		return "", err
	}
	return hashBytes(b), nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
