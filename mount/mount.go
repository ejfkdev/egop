// Package mount 是**一站式外接插件装配**:一份 Sources 声明同时驱动 wasm
// 目录装载(可选热更)、远程插件(出站/入站)——业务仓不再写任何加载循环,
// 只声明来源。远程通道本身不建连接:出站/入站的传输一律经注入的
// StreamDial/StreamAccept(`io/fs.FS` 同理),与本库"零平台绑定"一致。
//
// 装配语义(与 cordis 的"loader 一次性配置,失败隔离"对齐):
//   - 目录里单个坏包 = 告警跳过,不阻断;
//   - 出站远程是显式配置:握手/注册失败 = 整体装配失败(须注入 StreamDial);
//   - 入站注册流断连 = 自动卸载(OnClosed),重连可重注册。
package mount

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/ejfkdev/egop/autoload"
	"github.com/ejfkdev/egop/loader"
	"github.com/ejfkdev/egop/loader/remote"
)

// RemoteSpec 是出站远程插件的一行配置(id 用于校验远端清单防连错)。
type RemoteSpec struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

// Sources 是装载来源的整份声明(消费方等装配根自己的配置文件,或运行时构造)。
type Sources struct {
	// Dirs 是 wasm 插件目录(*.egop.wasm / *.egop.zip;顺序即装载优先级,先到先得)。
	Dirs []string `json:"dirs,omitempty"`
	// FS 注入的只读文件系统:非 nil 时 Dirs 视作该 FS 内的根目录(内嵌/浏览器用,
	// 例如 fstest.MapFS 或自定义实现);nil = 操作系统目录。
	FS fs.FS `json:"-"`
	// ExtraSuffixes 追加的插件 zip 包后缀(透传 wasm.Options.ExtraSuffixes;
	// 品牌/项目自有后缀由装配注入,内容无关库不内置业务词)。
	ExtraSuffixes []string `json:"extra_suffixes,omitempty"`
	// Watch 监视 Dirs 热更(增/改/删 → 注册/热替换/卸载);Interval 轮询周期(0=1s)。
	Watch    bool          `json:"watch,omitempty"`
	Interval time.Duration `json:"-"`
	// Remote 出站远程插件:框架主动连接插件(须配 StreamDial 建立传输)。
	Remote []RemoteSpec `json:"remote,omitempty"`
	// StreamAccept 注入的入站传输:每调返回一条已建立的入站流(同 net.Listener.Accept
	// 语义,但只给字节流)。非 nil 时框架在后台 accept 并 ServeStream。
	StreamAccept func(ctx context.Context) (remote.Stream, error) `json:"-"`
	// StreamDial 注入的出站传输:按址返回一条已建立的流。Remote 各 spec 经它拨。
	StreamDial func(ctx context.Context, addr string) (remote.Stream, error) `json:"-"`
	// Logf 生命周期日志;nil = 静默。
	Logf func(format string, args ...any)
}

// Runtime 是一次装配的运行句柄(Close 统一清退)。
type Runtime struct {
	hf       loader.HostFace
	src      Sources
	mu       sync.Mutex
	sessions []*remote.Session
	inbound  []remote.Stream // 入站已接受的流(Runtime 责任在 Close 时关闭)
	failed   bool            // 装配失败标记:Close 走全清(含目录插件反注册)
	watcher  *autoload.Watcher
	cancel   context.CancelFunc // 停止注入的入站 accept 循环
}

// Mount 按来源装配:目录三连拍两段确认装载、出站拨号、入站 accept;Watch 时
// 开热更轮询。返回 (运行句柄, 目录坏包告警, 装配错误——错误时句柄已自行全清:
// 停 watcher/会话/入站流,并反注册+关闭目录阶段已进册的插件,宿主回到装配前
// 状态,wasm 运行时句柄不泄漏)。
func Mount(ctx context.Context, hf loader.HostFace, src Sources) (*Runtime, []error, error) {
	rt := &Runtime{hf: hf, src: src}
	fail := func(err error) (*Runtime, []error, error) {
		rt.mu.Lock()
		rt.failed = true
		rt.mu.Unlock()
		rt.Close()
		return nil, nil, err
	}

	var warns []error
	w := autoload.New(hf, src.Dirs, autoload.Options{Interval: src.Interval, Logf: src.Logf, FS: src.FS, ExtraSuffixes: src.ExtraSuffixes})
	// 首批装载:两段确认语义下先三连拍落定,再继续拍到无新注册/替换为止——
	// 依赖链乱序时后落地件会跨轮补载,固定三拍会漏掉;apply 顺序本身受 map 遍历
	// 随机性影响,须以"无进展即停"判定,而非固定次数。
	var first []autoload.Event
	const maxInitRounds = 64
	for i := 0; i < maxInitRounds; i++ {
		evs := w.Poll(ctx)
		first = append(first, evs...)
		if i >= 3 {
			progressed := false
			for _, e := range evs {
				if e.Action == autoload.ActionRegister || e.Action == autoload.ActionReplace {
					progressed = true
					break
				}
			}
			if !progressed {
				break
			}
		}
	}
	// 瞬态失败(依赖尚未落地)随后成功者不再告警;真正坏件/契约不满足仍告警,
	// 且同一文件跨轮重复失败只告警一次。
	successIDs := map[string]bool{}
	for _, e := range first {
		if e.Action == autoload.ActionRegister || e.Action == autoload.ActionReplace {
			successIDs[e.PluginID] = true
		}
	}
	warnedPath := map[string]bool{}
	for _, e := range first {
		if e.Action != autoload.ActionFailed {
			continue
		}
		if e.PluginID != "" && successIDs[e.PluginID] {
			continue
		}
		if warnedPath[e.Path] {
			continue
		}
		warnedPath[e.Path] = true
		warns = append(warns, fmt.Errorf("wasm plugin %s: %w", e.Path, e.Err))
	}
	rt.watcher = w

	if len(src.Remote) > 0 && src.StreamDial == nil {
		return fail(fmt.Errorf("mount: remote plugins require injected StreamDial"))
	}
	for _, spec := range src.Remote {
		stream, err := src.StreamDial(ctx, spec.Addr)
		if err != nil {
			return fail(fmt.Errorf("mount: remote %s: %w", spec.ID, err))
		}
		adapter, sess, err := remote.DialStream(ctx, hf, stream, remote.DialOptions{WantID: spec.ID})
		if err != nil {
			// DialStream 失败时一律返回 (nil, nil, err) 且已自行关闭会话;
			// sess 必须判空,否则此处 nil.Close 会 panic。
			if sess != nil {
				sess.Close()
			}
			return fail(fmt.Errorf("mount: remote %s: %w", spec.ID, err))
		}
		if err := hf.Register(adapter); err != nil {
			sess.Close()
			return fail(fmt.Errorf("mount: remote %s: register: %w", spec.ID, err))
		}
		sess.OnClosed(func(err error) {
			if _, rerr := hf.Remove(adapter.Meta().ID, false); rerr != nil {
				rt.logf("remote: unregister %s: %v", adapter.Meta().ID, rerr)
			} else {
				rt.logf("remote: plugin %s unregistered (stream ended: %v)", adapter.Meta().ID, err)
			}
		})
		rt.sessions = append(rt.sessions, sess)
	}

	if src.StreamAccept != nil {
		acceptCtx, cancel := context.WithCancel(ctx)
		rt.cancel = cancel
		go func() {
			for {
				stream, err := src.StreamAccept(acceptCtx)
				if err != nil {
					return
				}
				rt.mu.Lock()
				rt.inbound = append(rt.inbound, stream)
				rt.mu.Unlock()
				go func() {
					_ = remote.ServeStream(acceptCtx, hf, stream, "", func(f string, a ...any) {
						rt.logf("remote: "+f, a...)
					})
				}()
			}
		}()
		rt.logf("remote: accepting inbound (injected transport)")
	}

	if src.Watch {
		w.Start(ctx)
	}
	return rt, warns, nil
}

func (rt *Runtime) logf(format string, args ...any) {
	if rt.src.Logf != nil {
		rt.src.Logf(format, args...)
	}
}

// Events 热更事件流(未开 Watch 时返回 nil;观察为可选)。
func (rt *Runtime) Events() <-chan autoload.Event {
	if rt.watcher == nil {
		return nil
	}
	return rt.watcher.Events()
}

// Close 统一清退:停热更、停入站 accept(并关闭已接受的入站流,传导对端 EOF)、
// 断出站会话(OnClosed 触发宿主卸载)。幂等。
// 装配失败路径(failed)额外做全清:目录阶段已进册的插件反注册并关闭句柄
// (级联带上其依赖者),宿主回到装配前状态,wasm 运行时不泄漏;正常关停不做
// 此步——注册面归宿主,由宿主总闸(Host.Close)统一清退。
func (rt *Runtime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.watcher != nil {
		rt.watcher.Stop()
		if rt.failed {
			rt.watcher.Unload(context.Background())
		}
	}
	if rt.cancel != nil {
		rt.cancel()
		rt.cancel = nil
	}
	for _, s := range rt.sessions {
		s := s
		go func() {
			_ = s.Shutdown("host closing")
			s.Close()
		}()
	}
	rt.sessions = nil
	for _, stream := range rt.inbound {
		_ = stream.Close()
	}
	rt.inbound = nil
}
