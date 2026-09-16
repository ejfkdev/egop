// Session 是单条双向流的复用引擎:请求↔回复按 id 关联,单向推送(push_event/shutdown)
// 与对端请求帧(HostCall/Subscribe/CallFunc 等)在 recvLoop 按 kind 分发给 peer。
// 两端(框架侧/插件侧)共用一个引擎,角色差异全在 peer 实现。传输无关:只依赖 Stream。
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ejfkdev/egop/contract"
)

// Peer 是连接对端的处理语义:框架侧 = 远程插件(我们向它发调用,它向我们要能力);
// 插件侧 = 框架(我们向它发 HostCall/Subscribe,它向我们要 call/tool/config)。
// 未实现的入口返回错误即可(对端不该发这类帧)。
type Peer interface {
	HandleCall(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error)
	HandleTool(ctx context.Context, tool string, args, tctx json.RawMessage) (json.RawMessage, error)
	HandleHook(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error)
	HandleApplyConfig(ctx context.Context, cfg json.RawMessage) error
	HandleHostCall(ctx context.Context, op string, input json.RawMessage) (json.RawMessage, error)
	HandleSubscribe(ctx context.Context, f *contract.EventFilter)
	HandleShutdown(reason string)
}

// UnimplementedPeer 是无操作的兜底 peer:一切入口返回"不该到达"错误。
type UnimplementedPeer struct{}

func (UnimplementedPeer) HandleCall(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: call not expected on this side")
}
func (UnimplementedPeer) HandleTool(context.Context, string, json.RawMessage, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: tool not expected on this side")
}
func (UnimplementedPeer) HandleHook(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: hook not expected on this side")
}
func (UnimplementedPeer) HandleApplyConfig(context.Context, json.RawMessage) error {
	return errors.New("remote: apply_config not expected on this side")
}
func (UnimplementedPeer) HandleHostCall(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("remote: host_call not expected on this side")
}
func (UnimplementedPeer) HandleSubscribe(context.Context, *contract.EventFilter) {}
func (UnimplementedPeer) HandleShutdown(string)                                  {}

// DefaultDispatchConcurrency 是单会话入站请求并发派发上限的默认值:
// 防 goroutine 无界(对端不可信,可廉价灌请求帧;无上界=无界 goroutine/内存),
// 超额回执 busy 给对端,背压而非阻塞读循环。量级取"合法高并发(慢处理器)
// 永远碰不到、只有病态洪水才碰"——1024 个阻塞 goroutine 约几 MB,可承受。
// 热点插件可按需调小/调大(DialOptions / WithDispatchConcurrency /
// mount RemoteSpec.dispatch_concurrency)。
const DefaultDispatchConcurrency = 1024

// Session 是单条流的双向复用引擎。
type Session struct {
	stream    Stream
	peer      atomic.Pointer[Peer]
	mu        sync.Mutex
	pending   map[uint64]chan *Frame
	nextID    atomic.Uint64
	done      chan struct{}
	closeOnce sync.Once
	onClosed  func(err error)
	pushFn    func(ctx context.Context, topic string, e contract.Event) // 插件侧事件投递口(构造期设置)
	cleanup   func()                                                    // 连接终结的资源清理(构造后设置)
	dispSem   chan struct{}                                             // 入站请求派发配额(有界并发)
}

// NewSession 包装一条双向流。
func NewSession(stream Stream) *Session {
	return &Session{
		stream:  stream,
		pending: map[uint64]chan *Frame{},
		done:    make(chan struct{}),
		dispSem: make(chan struct{}, DefaultDispatchConcurrency),
	}
}

// SetPeer 设置对端语义(握手完成前为 nil)。
func (s *Session) SetPeer(p Peer) { s.peer.Store(&p) }

// SetDispatchConcurrency 覆盖入站请求并发派发上限(**须在 Start 前调用**;
// n<=0 = 保持默认 DefaultDispatchConcurrency)。热点远程插件(慢处理器+
// 高并发调用方)按需调大;受控环境可调小。
func (s *Session) SetDispatchConcurrency(n int) {
	if n > 0 {
		s.dispSem = make(chan struct{}, n)
	}
}

// SetPushHandler 设置事件推送投递口(插件侧)。
func (s *Session) SetPushHandler(fn func(ctx context.Context, topic string, e contract.Event)) {
	s.mu.Lock()
	s.pushFn = fn
	s.mu.Unlock()
}

// OnClosed 连接死亡(断流/Shutdown)回调,只触发一次。
func (s *Session) OnClosed(cb func(err error)) {
	s.mu.Lock()
	s.onClosed = cb
	s.mu.Unlock()
}

// setCleanup 设置连接终结的资源清理回调(构造后可设置,与 closeWith 经 mu 同步)。
func (s *Session) setCleanup(fn func()) {
	s.mu.Lock()
	s.cleanup = fn
	s.mu.Unlock()
}

// Done 连接终结信号。
func (s *Session) Done() <-chan struct{} { return s.done }

// Start 启动接收循环(goroutine;握手前可用)。
func (s *Session) Start() { go s.recvLoop() }

// Send 单写出(全帧互斥)。
func (s *Session) Send(f *Frame) error { return s.send(f) }

func (s *Session) send(f *Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	select {
	case <-s.done:
		return errors.New("remote: session closed")
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(data)
}

// Request 发出请求帧并等回复(超时/cancel 由 ctx 控制)。
func (s *Session) Request(ctx context.Context, f *Frame) (*Frame, error) {
	id := s.nextID.Add(1)
	f.Id = id
	ch := make(chan *Frame, 1)
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil, errors.New("remote: session closed")
	default:
	}
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.send(f); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, errors.New("remote: session closed")
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Register 发送注册帧并等回执(回执 frame 的 Manifest = 最终生效清单)。
func (s *Session) Register(ctx context.Context, manifest json.RawMessage, token string) (*Frame, error) {
	rep, err := s.Request(ctx, &Frame{Kind: KindRegister, Manifest: manifest, Token: token})
	if err != nil {
		return nil, err
	}
	if rep.Error != "" {
		return nil, errors.New(rep.Error)
	}
	return rep, nil
}

// CallFunc 框架→插件:调用插件函数(回复 payload 为信封)。ctx 里的调用来源
// (contract.OriginFrom)摘出随帧上线,插件侧还原进处理 ctx——与 wasm ABI 的
// egop_call 第三参同款语义(ctx 值不跨边界)。
func (s *Session) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return s.requestPayload(ctx, &Frame{Kind: KindCallFunc, Fname: fname, Input: input, Origin: contract.OriginFrom(ctx)})
}

// Tool 框架→插件:工具调用。
func (s *Session) Tool(ctx context.Context, tool string, args, tctx json.RawMessage) (json.RawMessage, error) {
	return s.requestPayload(ctx, &Frame{Kind: KindTool, Name: tool, Input: args, Tctx: tctx})
}

// Hook 框架→插件:hook 回调触发(回复 payload 为 HookResult JSON 信封)。
// 触发来源随帧上线(同 CallFunc)。
func (s *Session) Hook(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error) {
	return s.requestPayload(ctx, &Frame{Kind: KindHook, HookID: hookID, Input: data, Origin: contract.OriginFrom(ctx)})
}

// ApplyConfig 框架→插件:配置下发。
func (s *Session) ApplyConfig(ctx context.Context, cfg json.RawMessage) error {
	_, err := s.requestPayload(ctx, &Frame{Kind: KindApplyConfig, Config: cfg})
	return err
}

// HostCall 插件→框架:能力回程(回复 payload 为信封)。
func (s *Session) HostCall(ctx context.Context, op string, input json.RawMessage) (json.RawMessage, error) {
	return s.requestPayload(ctx, &Frame{Kind: KindHostCall, Op: op, Input: input})
}

// Subscribe 插件→框架:按过滤条件订阅(nil/零值 = 订阅所有事件)。
func (s *Session) Subscribe(ctx context.Context, f *contract.EventFilter) error {
	_, err := s.requestPayload(ctx, &Frame{Kind: KindSubscribe, Filter: f})
	return err
}

// Ping 保活/延迟探针。
func (s *Session) Ping(ctx context.Context) error {
	_, err := s.requestPayload(ctx, &Frame{Kind: KindPing})
	return err
}

// requestPayload 发请求并解信封回复。
func (s *Session) requestPayload(ctx context.Context, f *Frame) (json.RawMessage, error) {
	rep, err := s.Request(ctx, f)
	if err != nil {
		return nil, err
	}
	if rep.Error != "" {
		return nil, errors.New(rep.Error)
	}
	return decodeEnvelope(rep.Payload)
}

// PushEvent 框架→插件:事件推送(单向帧,承载完整 Event)。
func (s *Session) PushEvent(e contract.Event) error {
	return s.send(&Frame{Kind: KindPushEvent, Topic: e.Type, SubType: e.SubType, Version: e.Version, Labels: e.Labels, Source: e.Source, Payload: e.Payload})
}

// Shutdown 通知对端收起(单向帧)。
func (s *Session) Shutdown(reason string) error {
	return s.send(&Frame{Kind: KindShutdown, Reason: reason})
}

// Close 主动终结本侧(不发送对端通知;先 Shutdown 再 Close 是礼貌用法)。
func (s *Session) Close() { s.closeWith(nil) }

func (s *Session) closeWith(err error) {
	s.closeOnce.Do(func() {
		close(s.done)
		var cleanup func()
		var onClosed func(error)
		s.mu.Lock()
		// 只摘除 pending 条目;不 close 通道——done 已先行关闭,阻塞中的 Request
		// 会经 <-done 醒来,而 close 空通道会让其 select 以 (nil Frame) 命中
		// ->ch 分支,可能被误当成功回复。
		for id := range s.pending {
			delete(s.pending, id)
		}
		cleanup = s.cleanup
		onClosed = s.onClosed
		// 关闭底层传输:与持锁写流的 send 串行(同 mu),杜绝写到"正在被关闭"的传输;
		// 把本侧关闭传导为对端 EOF(触发对端自动卸载)。
		_ = s.stream.Close()
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		if onClosed != nil {
			onClosed(err)
		}
	})
}

func (s *Session) takePending(id uint64) chan *Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.pending[id]
	delete(s.pending, id)
	return ch
}

func (s *Session) replyTo(ctx context.Context, id uint64, payload json.RawMessage) {
	_ = s.send(&Frame{Id: id, Reply: true, Payload: payload})
}

func (s *Session) replyErr(id uint64, msg string) {
	_ = s.send(&Frame{Id: id, Reply: true, Error: msg, Payload: errEnvelope(errors.New(msg))})
}

// dispatchAsync 把入站请求帧派发到带界工作 goroutine:一个慢 op(长调用/挂死
// 的回调)不再队头阻塞整条会话——后续帧(含其它请求的回复路由、事件推送)
// 继续处理;同会话自调(插件经宿主回程调回自己)也因此不再死锁:外层处理
// 等回复期间,嵌套请求帧照常读入派发。配额满即回执 busy(背压给对端,绝不
// 阻塞读循环)。副作用:入站请求的**处理不再保序**(帧仍按序读入;回复按 id
// 关联,顺序无依赖)。事件推送(push_event)保持内联派发以保投递顺序。
func (s *Session) dispatchAsync(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: request before handshake")
		return
	}
	select {
	case s.dispSem <- struct{}{}:
	default:
		s.replyErr(f.Id, "remote: dispatcher busy (too many concurrent requests)")
		return
	}
	fc := *f // 帧拷贝:f 是循环局部变量,下一轮复用
	go func() {
		defer func() { <-s.dispSem }()
		switch fc.Kind {
		case KindCallFunc:
			s.dispatchCall(ctx, peer, &fc)
		case KindTool:
			s.dispatchTool(ctx, peer, &fc)
		case KindHook:
			s.dispatchHook(ctx, peer, &fc)
		case KindApplyConfig:
			s.dispatchConfig(ctx, peer, &fc)
		case KindHostCall:
			s.dispatchHostCall(ctx, peer, &fc)
		}
	}()
}

// recvLoop 是流的唯一读侧:回复路由回 pending,请求/单向帧按 kind 分发 peer。
func (s *Session) recvLoop() {
	var loopErr error
	defer func() { s.closeWith(loopErr) }()
	ctx := s.stream.Context()
	for {
		data, err := s.stream.Recv()
		if err != nil {
			loopErr = err
			return
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			loopErr = err
			return
		}
		var peer Peer
		if ptr := s.peer.Load(); ptr != nil {
			peer = *ptr
		}
		switch {
		case f.Reply:
			if ch := s.takePending(f.Id); ch != nil {
				ch <- &f
			}
		case f.Id != 0:
			switch f.Kind {
			case KindCallFunc, KindTool, KindHook, KindApplyConfig, KindHostCall:
				s.dispatchAsync(ctx, peer, &f)
			case KindSubscribe:
				if peer == nil {
					s.replyErr(f.Id, "remote: subscribe before handshake")
					continue
				}
				peer.HandleSubscribe(ctx, f.Filter)
				s.replyTo(ctx, f.Id, okEnvelope(json.RawMessage("null")))
			case KindPing:
				s.replyTo(ctx, f.Id, okEnvelope(json.RawMessage("null")))
			default:
				s.replyErr(f.Id, "remote: unsupported request frame")
			}
		default:
			switch f.Kind {
			case KindPushEvent:
				s.mu.Lock()
				fn := s.pushFn
				s.mu.Unlock()
				if fn != nil {
					fn(ctx, f.Topic, contract.Event{
						Type:    f.Topic,
						SubType: f.SubType,
						Version: f.Version,
						Source:  f.Source,
						Payload: f.Payload,
						Labels:  f.Labels,
					})
				}
			case KindShutdown:
				if peer != nil {
					peer.HandleShutdown(f.Reason)
				}
				loopErr = fmt.Errorf("remote shutdown: %s", f.Reason)
				return
			}
		}
	}
}

// peerCall 执行 peer 处理器并把 panic 转成 error(入站调用边界 fail-closed:插件/对端
// 处理器炸了由 egop 归一到错误回执,不 crash 会话)。
func peerCall(fn func() (json.RawMessage, error)) (out json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			out = nil
			err = fmt.Errorf("remote: handler panic: %v", p)
		}
	}()
	return fn()
}

// peerCallErr 同 peerCall,但处理器只返回 error。
func peerCallErr(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("remote: handler panic: %v", p)
		}
	}()
	return fn()
}

// withFrameOrigin 把帧上线的调用来源还原进处理 ctx(插件侧经 OriginFrom 读回
// "谁调了我/哪个点触发";无来源帧原样返回)。
func withFrameOrigin(ctx context.Context, f *Frame) context.Context {
	if f.Origin == nil {
		return ctx
	}
	return contract.WithOrigin(ctx, f.Origin)
}

func (s *Session) dispatchCall(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: call before handshake")
		return
	}
	ctx = withFrameOrigin(ctx, f)
	out, err := peerCall(func() (json.RawMessage, error) { return peer.HandleCall(ctx, f.Fname, f.Input) })
	if err != nil {
		s.replyErr(f.Id, err.Error())
		return
	}
	s.replyTo(ctx, f.Id, okEnvelope(out))
}

func (s *Session) dispatchTool(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: tool before handshake")
		return
	}
	out, err := peerCall(func() (json.RawMessage, error) { return peer.HandleTool(ctx, f.Name, f.Input, f.Tctx) })
	if err != nil {
		s.replyErr(f.Id, err.Error())
		return
	}
	s.replyTo(ctx, f.Id, okEnvelope(out))
}

func (s *Session) dispatchHook(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: hook before handshake")
		return
	}
	ctx = withFrameOrigin(ctx, f)
	out, err := peerCall(func() (json.RawMessage, error) { return peer.HandleHook(ctx, f.HookID, f.Input) })
	if err != nil {
		s.replyErr(f.Id, err.Error())
		return
	}
	s.replyTo(ctx, f.Id, okEnvelope(out))
}

func (s *Session) dispatchConfig(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: apply_config before handshake")
		return
	}
	if err := peerCallErr(func() error { return peer.HandleApplyConfig(ctx, f.Config) }); err != nil {
		s.replyErr(f.Id, err.Error())
		return
	}
	s.replyTo(ctx, f.Id, okEnvelope(json.RawMessage("null")))
}

func (s *Session) dispatchHostCall(ctx context.Context, peer Peer, f *Frame) {
	if peer == nil {
		s.replyErr(f.Id, "remote: host_call before handshake")
		return
	}
	out, err := peerCall(func() (json.RawMessage, error) { return peer.HandleHostCall(ctx, f.Op, f.Input) })
	if err != nil {
		s.replyErr(f.Id, err.Error())
		return
	}
	s.replyTo(ctx, f.Id, okEnvelope(out))
}
