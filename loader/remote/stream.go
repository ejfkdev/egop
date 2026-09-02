// 传输无关的远程通道缝:egop 只在这条流上收发 JSON 帧,不建立、也不关心底层连接
// ——http/https/websocket/浏览器通道/任意字节流由外部注入(与 io/fs.FS 注入缝同理)。
//
// Stream 是双向字节消息流的最小面;BindStream 用 4 字节大端长度前缀把裸字节流
// 帧化;Frame 是流上唯一的交换结构(JSON,与全库"唯一编码"一致):id 关联请求/回复,
// kind 标识操作,payload 承载结果信封(回复)或事件载荷(单向 push_event)。
package remote

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/ejfkdev/egop/contract"
)

// Stream 是双向字节消息流的最小面:外部 transport 实现 Send/Recv/Context/Close 即可。
// 每个 Send 的消息 = 一帧 JSON;BindStream 用长度前缀在裸字节流上做强帧。
// Close 显式进接口:会话收尾统一能关底层传输(传导对端 EOF),非可关的实现至少返
// 一个 error / 空操作,不能再依赖可选接口断言。
type Stream interface {
	Send([]byte) error
	Recv() ([]byte, error)
	Context() context.Context
	Close() error
}

// maxFrameSize 单帧上限(防御性)。
const maxFrameSize = 64 << 20 // 64MiB

// ErrFrameTooLarge 表示对端发来的帧超过 maxFrameSize(错误归类为"帧过大",而非误报 EOF)。
var ErrFrameTooLarge = errors.New("remote: frame too large")

// BindStream 把一段字节双向流(rw)适配成 Stream(4 字节大端长度前缀 + 载荷)。
// 外部 transport 只需提供读写字节(如 net.Conn、WebSocket 包装、HTTP/2 双向体、
// 浏览器消息通道)。
func BindStream(ctx context.Context, rw io.ReadWriteCloser) Stream {
	return &byteStream{ctx: ctx, rw: rw}
}

type byteStream struct {
	ctx context.Context
	rw  io.ReadWriteCloser
	mu  sync.Mutex // Send 串行化(底层写无并发安全)
}

func (b *byteStream) Send(data []byte) error {
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], data)
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.rw.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) { // 短写即帧被截断(长度前缀与载荷失衡),显式报错而非常默认 lenient 传输吞掉。
		return io.ErrShortWrite
	}
	return nil
}

func (b *byteStream) Recv() ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(b.rw, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxFrameSize {
		return nil, ErrFrameTooLarge
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(b.rw, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (b *byteStream) Context() context.Context { return b.ctx }

// Close 关闭底层字节流:Session 收尾时触发,把"本侧关闭"传导为对端 EOF,
// 从而触发对端自动卸载。
func (b *byteStream) Close() error { return b.rw.Close() }

// 操作类型(kind)。
const (
	KindRegister    = "register"
	KindCallFunc    = "call_func"
	KindTool        = "tool"
	KindHook        = "hook"
	KindApplyConfig = "apply_config"
	KindHostCall    = "host_call"
	KindSubscribe   = "subscribe"
	KindPushEvent   = "push_event"
	KindShutdown    = "shutdown"
	KindPing        = "ping"
)

// Frame 是流上唯一的交换结构(JSON)。各操作参数按 kind 平铺为字段。
type Frame struct {
	Id       uint64                `json:"id,omitempty"`    // 请求/回复关联;单向帧 id=0
	Reply    bool                  `json:"reply,omitempty"` // true = 对同 id 请求的响应
	Kind     string                `json:"kind,omitempty"`
	Error    string                `json:"error,omitempty"`    // 传输级/校验失败
	Payload  json.RawMessage       `json:"payload,omitempty"`  // 结果信封(回复) / push_event 载荷
	Manifest json.RawMessage       `json:"manifest,omitempty"` // register
	Token    string                `json:"token,omitempty"`    // register
	Fname    string                `json:"fname,omitempty"`    // call_func
	Input    json.RawMessage       `json:"input,omitempty"`    // call_func / tool / hook / host_call
	Name     string                `json:"name,omitempty"`     // tool 名
	Tctx     json.RawMessage       `json:"tctx,omitempty"`     // tool 上下文
	HookID   string                `json:"hook_id,omitempty"`  // hook
	Op       string                `json:"op,omitempty"`       // host_call
	Topic    string                `json:"topic,omitempty"`    // push_event 主题(事件 Type)
	SubType  string                `json:"sub_type,omitempty"` // publish_event / push_event 子类型
	Version  int                   `json:"version,omitempty"`  // push_event 信封版本(事件 Version)
	Labels   map[string]string     `json:"labels,omitempty"`   // publish_event / push_event 标签
	Source   *contract.Origin      `json:"source,omitempty"`   // push_event 来源(Origin)
	Filter   *contract.EventFilter `json:"filter,omitempty"`   // subscribe 过滤条件
	Config   json.RawMessage       `json:"config,omitempty"`   // apply_config
	Reason   string                `json:"reason,omitempty"`   // shutdown
	// Origin 是 call_func/hook 帧的调用来源(与 wasm ABI 的第三参同款语义:ctx 值
	// 不跨边界,框架侧从 ctx 摘出上线,插件侧还原进处理 ctx)。旧对端不认识该字段
	// 即自然忽略(加性演进,不破坏线上兼容)。
	Origin *contract.Origin `json:"origin,omitempty"`
}

// marshal 把 Frame 编成一帧 JSON 字节。
func (f *Frame) marshal() ([]byte, error) { return json.Marshal(f) }

// unmarshalFrame 解一帧 JSON 字节为 Frame。
func unmarshalFrame(data []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// sendFrame 把一个 Frame 序列化后发到字节流。
func sendFrame(stream Stream, f *Frame) error {
	data, err := f.marshal()
	if err != nil {
		return err
	}
	return stream.Send(data)
}

// recvFrame 从字节流读出一帧。
func recvFrame(stream Stream) (*Frame, error) {
	data, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	return unmarshalFrame(data)
}
