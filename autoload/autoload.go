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
	"errors"
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
	// ExtraSuffixes 追加的插件 zip 包后缀(透传 wasm.Options.ExtraSuffixes;
	// 品牌/项目自有后缀由装配注入,内容无关库不内置业务词)。
	ExtraSuffixes []string
}

// dot 是一个目录下的一行文件观察。
type unit struct {
	path    string // 规格化路径(相对原样,供事件展示)
	hash    string // 当前已加载内容 hash
	pending string // 上轮观察到的候选 hash(两段确认:增/改)
	misses  int    // 连续未见轮数(两段确认:删;瞬态目录读失败不误卸)
	id      string // 在册插件 id(manifest)
	plugin  *wasm.Plugin
	loaded  bool
	read    func() ([]byte, error) // 读取当前内容(os 目录或注入 FS)

	// 失败重试分层(与决策 #25 配套):
	np           *wasm.Plugin // 装载成功、应用未成的候选——重试应用,不重编译
	npHash       string       // np 对应内容 hash(变化即作废重装)
	npRegistered bool         // 换 id 时 np 已入册、旧 id 仍权威;清退需先反注册 np
	errHash      string       // 内容坏件的 hash——未变化不重试(重试=每轮全量重编译)
}

// fileSig 文件身份(大小+修改时间)。轮询以它做廉价变更门:未变不读文件内容,
// 只 stat。此前每秒对全部插件包做 sha256(几十 MB/s),是空闲 CPU 的主要来源。
type fileSig struct {
	size int64
	mod  int64
	hash string // 该签名下已算出的内容 hash(签名变即失效)
}

// Watcher 监视一组插件目录并把变更应用到宿主。
type Watcher struct {
	hf    loader.HostFace
	dirs  []string
	opts  Options
	fsys  fs.FS // 注入的只读文件系统;nil = 操作系统目录
	mu    sync.Mutex
	units map[string]*unit   // path → 观察单元(含已加载与 pending)
	sigs  map[string]fileSig // path → 最近一次 stat 签名与 hash(未变即复用)
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
		sigs:  map[string]fileSig{},
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

// discardCandidate 关闭未应用候选。换 id 回滚清理未成的极端情况下候选可能
// 仍在册;必须先反注册再 Close,且保留失败时的所有权记录供下轮继续清退。
func (w *Watcher) discardCandidate(ctx context.Context, u *unit) error {
	if u.np == nil {
		u.npHash, u.npRegistered = "", false
		return nil
	}
	if u.npRegistered {
		id := u.np.Meta().ID
		if _, err := w.hf.Remove(id, true); err != nil {
			return fmt.Errorf("rollback registered candidate %q: %w", id, err)
		}
		u.npRegistered = false
	}
	_ = u.np.Close(ctx)
	u.np, u.npHash = nil, ""
	return nil
}

// Unload 反注册并关闭全部已加载插件(mount 装配失败的全清路径:级联卸载带上
// 依赖者,句柄逐个 Close,wasm 运行时不泄漏)。Stop 之后调用;幂等——已清空的
// 观察表再调是空操作。正常关停不走这里(注册面归宿主总闸 Host.Close)。
func (w *Watcher) Unload(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, u := range w.units {
		if u.np != nil {
			if err := w.discardCandidate(ctx, u); err != nil {
				w.logf("autoload: unload candidate %s: %v", p, err)
				continue // 注册面与句柄均保留在 unit,下次 Unload 可继续清退
			}
		}
		if !u.loaded {
			delete(w.units, p)
			continue
		}
		if _, err := w.hf.Remove(u.id, true); err != nil {
			w.logf("autoload: unload %s: %v", u.id, err)
		}
		if u.plugin != nil {
			_ = u.plugin.Close(ctx)
		}
		delete(w.units, p)
	}
}

// Poll 显式驱动一轮(装配层可与 Start 混用;并发调用互斥)。
func (w *Watcher) Poll(ctx context.Context) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pollOnceLocked(ctx)
}

// PollStable 在同一互斥区内驱动两轮确认,后台或其他显式轮询不能插入两轮之间。
func (w *Watcher) PollStable(ctx context.Context) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	events := w.pollOnceLocked(ctx)
	return append(events, w.pollOnceLocked(ctx)...)
}

// ArtifactState 返回目录装载插件当前已应用的工件路径与内容 hash。
// 管理面据此区分“文件已落盘”与“这份字节已经由 watcher 应用”，不依赖事件恰好被某次调用看到。
func (w *Watcher) ArtifactState(pluginID string) (path, hash string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, u := range w.units {
		if u.loaded && u.id == pluginID {
			return path, u.hash, true
		}
	}
	return "", "", false
}

// PathFor 返回目录装载插件当前生效的工件路径；内建、远程和待补载件不在此目录账中。
func (w *Watcher) PathFor(pluginID string) (string, bool) {
	path, _, ok := w.ArtifactState(pluginID)
	return path, ok
}

// Reload 强制从目录工件重建一个已装载插件。只使目标单元的已应用 hash 失效,
// 随后仍走普通两轮确认与 applyChange,因此未变工件也会得到新实例,坏替换保留旧版,
// 配置回放与远程/内建插件边界不发生分叉。
func (w *Watcher) Reload(ctx context.Context, pluginID string) ([]Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	found := false
	for _, u := range w.units {
		if !u.loaded || u.id != pluginID {
			continue
		}
		if u.np != nil {
			if err := w.discardCandidate(ctx, u); err != nil {
				return nil, err // Reload 必须重建,不能误用上次失败的保留候选
			}
		}
		u.hash = ""
		u.pending = ""
		u.errHash = ""
		found = true
	}
	if !found {
		return nil, fmt.Errorf("autoload: plugin %q is not directory-mounted", pluginID)
	}
	events := w.pollOnceLocked(ctx)
	events = append(events, w.pollOnceLocked(ctx)...)
	return events, nil
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
			if !wasm.IsPluginFile(d.Name(), w.opts.ExtraSuffixes) {
				return nil
			}
			wpath := w.keyFor(dir, root, path)
			st, err := fs.Stat(fsys, path)
			if err != nil {
				return nil // 半截写/瞬时读失败:下一轮再试
			}
			var h string
			prev, cached := w.sigs[wpath]
			// mtime 为零(注入 FS 常见)不可信 → 每轮仍 hash;真实文件走签名命中。
			if cached && prev.hash != "" && prev.size == st.Size() &&
				!st.ModTime().IsZero() && prev.mod == st.ModTime().UnixNano() {
				h = prev.hash
			} else {
				h, err = hashFS(fsys, path)
				if err != nil {
					return nil
				}
				w.sigs[wpath] = fileSig{size: st.Size(), mod: st.ModTime().UnixNano(), hash: h}
			}
			seen[wpath] = h
			read[wpath] = func() ([]byte, error) { return fs.ReadFile(fsys, path) }
			return nil
		})
	}
	// 1. 删除:两段确认(连续两轮未见才算真删,与增/改同哲学)——目录瞬态读失败、
	// 文件系统抖动不误卸已加载插件;失败/待应用单元也在确认后关实例并丢弃。
	for p, u := range w.units {
		if _, ok := seen[p]; ok {
			u.misses = 0
			continue
		}
		u.misses++
		if u.misses < 2 {
			continue // 首轮未见:待下一轮确认
		}
		if !u.loaded {
			id := u.id
			if u.np != nil && id == "" {
				id = u.np.Meta().ID
			}
			if err := w.discardCandidate(ctx, u); err != nil {
				u.misses = 0
				events = append(events, Event{Action: ActionFailed, PluginID: id, Path: p, Err: err})
				continue
			}
			if u.plugin != nil {
				_ = u.plugin.Close(ctx)
			}
			delete(w.units, p)
			delete(w.sigs, p)
			events = append(events, Event{Action: ActionRemove, PluginID: id, Path: p})
			continue
		}
		if err := w.discardCandidate(ctx, u); err != nil {
			u.misses = 0
			events = append(events, Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err})
			continue
		}
		ev := Event{Action: ActionRemove, PluginID: u.id, Path: p}
		if _, err := w.hf.Remove(u.id, false); err != nil {
			ev.Err = err // 被依赖:保留在册(插件继续服务),仅告警
			u.misses = 0 // 依赖解除后需重新两段确认,不每轮重复告警
		} else {
			_ = u.plugin.Close(ctx)
			delete(w.units, p)
		}
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
			if u.np != nil {
				// 文件回滚到在役内容:未应用候选必须立即清退,不能陪着旧 hash 永久滞留。
				if err := w.discardCandidate(ctx, u); err != nil {
					events = append(events, Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err})
					continue
				}
				u.pending = ""
			}
			continue // 无变化
		}
		if u.np == nil && u.errHash == h {
			continue // 内容坏且未变:不重试(等 hash 变化;每轮重编译是纯 CPU 烧)
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

// applyChange 装载新内容和应用(入册/替换);回退语义见包注释。失败重试分层:
//   - LoadFS 失败:记 errHash——hash 未变不重试(重试=每轮全量重编译,
//     纯 CPU 烧);内容变化后重新两段确认再试。
//   - 宿主瞬态拒绝:保留已装载候选 np——后续轮次只重试 Register/Replace,
//     不把暂时失败固化为该 hash 的永久坏件。
func (w *Watcher) applyChange(ctx context.Context, u *unit, h, p string, _ bool) Event {
	if u.np != nil {
		if h != u.npHash {
			// 内容已变:旧候选(含换 id 回滚未净的在册候选)先作废。
			if err := w.discardCandidate(ctx, u); err != nil {
				return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err}
			}
		} else {
			if !u.loaded {
				return w.registerCandidate(u, u.np, h, p)
			}
			if u.np.Meta().ID != u.id {
				return w.reidentify(ctx, u, u.np, h, p)
			}
			return w.replaceSameID(ctx, u, u.np, h, p)
		}
	}
	data, err := u.read()
	if err != nil {
		u.pending = ""
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err}
	}
	actualHash := hashBytes(data)
	if actualHash != h {
		// The confirmed bytes and the bytes about to be loaded are different.
		// Drop the observation and require two fresh confirmations of the new content.
		delete(w.sigs, p)
		u.pending = ""
		return Event{Action: ActionFailed, PluginID: u.id, Path: p,
			Err: fmt.Errorf("content changed during load: confirmed hash %s, read hash %s", h, actualHash)}
	}
	np, err := wasm.LoadFS(ctx, data, filepath.Base(p), wasm.Options{ExtraSuffixes: w.opts.ExtraSuffixes})
	if err != nil {
		u.pending = ""
		u.errHash = h // 坏新增与坏替换都等 hash 变化再试;在役旧版继续服务。
		// 装载失败一律 ActionFailed(含"旧版在册的坏替换"——旧版继续服务)。
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: err}
	}
	u.errHash = ""
	if !u.loaded {
		return w.registerCandidate(u, np, h, p)
	}
	if np.Meta().ID != u.id {
		return w.reidentify(ctx, u, np, h, p)
	}
	return w.replaceSameID(ctx, u, np, h, p)
}

func (w *Watcher) registerCandidate(u *unit, np *wasm.Plugin, h, p string) Event {
	meta := np.Meta()
	if err := w.hf.Register(np); err != nil {
		// 依赖未就位等瞬态失败:保留已装载实例,下一轮只重试 Register。
		u.np, u.npHash, u.npRegistered = np, h, false
		return Event{Action: ActionFailed, PluginID: meta.ID, Path: p, Err: err}
	}
	u.id, u.hash, u.loaded, u.plugin = meta.ID, h, true, np
	u.np, u.npHash, u.npRegistered = nil, "", false
	w.logf("autoload: plugin %s registered (%s)", meta.ID, p)
	return Event{Action: ActionRegister, PluginID: meta.ID, Path: p, Version: meta.Version}
}

func (w *Watcher) replaceSameID(ctx context.Context, u *unit, np *wasm.Plugin, h, p string) Event {
	meta := np.Meta()
	old := u.plugin
	prevCfg, hadCfg := w.hf.AppliedConfig(u.id)
	if err := w.hf.Replace(np); err != nil {
		// 瞬态失败不 Close、不记 errHash:同一候选下一轮直接重试 Replace。
		u.np, u.npHash, u.npRegistered = np, h, false
		u.errHash = ""
		return Event{Action: ActionFailed, PluginID: u.id, Path: p, Err: fmt.Errorf("replace refused, old version kept: %w", err)}
	}
	_ = old.Close(ctx)
	u.hash, u.plugin = h, np
	u.np, u.npHash, u.npRegistered = nil, "", false
	u.errHash = ""
	var replayErr error
	if hadCfg {
		if err := w.hf.SetConfig(u.id, prevCfg); err != nil {
			replayErr = fmt.Errorf("config replay: %w", err)
		}
	}
	w.logf("autoload: plugin %s replaced (%s %s)", u.id, p, meta.Version)
	return Event{Action: ActionReplace, PluginID: u.id, Path: p, Version: meta.Version, Err: replayErr}
}

func (w *Watcher) reidentify(ctx context.Context, u *unit, np *wasm.Plugin, h, p string) Event {
	meta := np.Meta()
	ev := Event{Action: ActionReplace, PluginID: u.id, Path: p, Version: meta.Version}
	if !u.npRegistered {
		if err := w.hf.Register(np); err != nil {
			// 依赖未就位等注册失败:保留同一候选,后续轮次只重试 Register。
			u.np, u.npHash, u.npRegistered = np, h, false
			ev.Action = ActionFailed
			ev.Err = fmt.Errorf("new manifest id %q: %w", meta.ID, err)
			return ev
		}
	}
	// 新 id 已入册后,在旧 id 删除结果落定前始终由 unit 记录候选所有权。
	u.np, u.npHash, u.npRegistered = np, h, true
	oldID := u.id
	if _, err := w.hf.Remove(oldID, false); err != nil {
		// 旧 id 仍是权威实例。反注册并关闭新候选;若回滚本身暂时失败,
		// discardCandidate 保留 npRegistered,后续轮次继续清退而不丢失所有权。
		rollbackErr := w.discardCandidate(ctx, u)
		u.pending = ""
		if u.np == nil {
			// 候选已完整撤销:同 hash 不再反复注册/初始化;内容变化或显式重载再试。
			u.errHash = h
		}
		ev.Action = ActionFailed
		ev.Err = errors.Join(fmt.Errorf("old manifest id %q could not be removed: %w", oldID, err), rollbackErr)
		return ev
	}
	old := u.plugin
	u.np, u.npHash, u.npRegistered = nil, "", false
	_ = old.Close(ctx)
	u.id, u.hash, u.plugin = meta.ID, h, np
	u.errHash = ""
	w.logf("autoload: plugin %s re-identified (was %s)", meta.ID, oldID)
	return ev
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
