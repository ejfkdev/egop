// 传输无关的握手面:egop 只负责在一条 remote.Stream 上做注册握手与会话接线,
// 不建立、也不关心底层连接——http/https/websocket/浏览器通道/任意字节流由外部
// 注入(BindStream 把字节流包成 Stream)。
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// DialOptions 是出站握手可选项。
type DialOptions struct {
	// WantID 非空时校验插件清单 id(防连错服务)。
	WantID string
}

func streamReplyErr(stream Stream, id uint64, msg string) error {
	return sendFrame(stream, &Frame{Id: id, Reply: true, Error: msg, Payload: errEnvelope(errors.New(msg))})
}

// AttachOption 是 AttachStream 的握手可选项(功能选项:加新可选项不改签名,
// 现有调用方零改动)。注意 AttachStream**不建连接**,只在既有 Stream 上做帧握手——
// 底层连接(含 HTTP/WebSocket upgrade 请求头等)由外部 transport 在建立 Stream 前
// 自由配置,egop 不接管这些传输细节。
type AttachOption func(*attachConfig)

type attachConfig struct {
	token string
}

// WithToken 设置帧级握手口令:框架侧 ServeStream 以同 token 校验;空 = 双方都不校验。
func WithToken(token string) AttachOption {
	return func(c *attachConfig) { c.token = token }
}

// AttachStream 是插件侧的传输无关握手:在既有 Stream 上先发 Register(manifest),
// 收框架回执,返回驱动中的会话。external transport 只需给一条 Stream。
// 可选功能选项(...AttachOption)扩展握手参数,如 WithToken(token)。
func AttachStream(ctx context.Context, stream Stream, mf contract.Manifest, ops *PluginOps, opts ...AttachOption) (*Session, error) {
	var cfg attachConfig
	for _, o := range opts {
		o(&cfg)
	}
	sess := NewSession(stream)
	if ops == nil {
		ops = &PluginOps{}
	}
	sess.SetPeer(&pluginPeer{ops: ops})
	if ops.PushEvent != nil {
		sess.SetPushHandler(ops.PushEvent)
	}
	sess.Start()
	manifestJSON, err := json.Marshal(mf)
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	reply, err := sess.Register(ctx, manifestJSON, cfg.token)
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("remote register: %w", err)
	}
	if reply.Kind != KindRegister {
		sess.Close()
		return nil, fmt.Errorf("remote register: bad reply")
	}
	return sess, nil
}

// DialStream 是框架出站的传输无关握手:在既有 Stream 上发 Register(manifest 空),
// 插件以 Register(manifest=最终清单) 回执,返回未进册的 Adapter 与会话。
func DialStream(ctx context.Context, rh RemoteHost, stream Stream, opts DialOptions) (*Adapter, *Session, error) {
	sess := NewSession(stream)
	sess.Start()
	reply, err := sess.Register(ctx, json.RawMessage("null"), "")
	if err != nil {
		sess.Close()
		return nil, nil, fmt.Errorf("remote register: %w", err)
	}
	if reply.Kind != KindRegister {
		sess.Close()
		return nil, nil, fmt.Errorf("remote register: bad reply (no manifest)")
	}
	var mf contract.Manifest
	if err := json.Unmarshal(reply.Manifest, &mf); err != nil {
		sess.Close()
		return nil, nil, fmt.Errorf("remote register: bad manifest: %w", err)
	}
	if mf.ID == "" {
		sess.Close()
		return nil, nil, fmt.Errorf("remote register: manifest id required")
	}
	if opts.WantID != "" && mf.ID != opts.WantID {
		sess.Close()
		return nil, nil, fmt.Errorf("remote register: manifest id %q != configured %q", mf.ID, opts.WantID)
	}
	hp := newHostPeer(rh, mf.ID)
	hp.SetPush(func(e contract.Event) { _ = sess.PushEvent(e) })
	hp.SetHook(func(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error) {
		return sess.Hook(ctx, hookID, data)
	})
	sess.SetPeer(hp)
	sess.setCleanup(hp.UnsubAll)
	return NewAdapter(sess, mf), sess, nil
}

// ServePluginStream 是插件侧作为**被拨入方**(出站方向)的传输无关握手:收框架
// 先发的 Register(manifest 空),以 Register(manifest=最终清单) 回执,驱动会话。
func ServePluginStream(ctx context.Context, stream Stream, mf contract.Manifest, ops *PluginOps) error {
	f, err := recvFrame(stream)
	if err != nil {
		return err
	}
	if f.Kind != KindRegister {
		_ = streamReplyErr(stream, f.Id, "remote: first frame must be Register")
		return nil
	}
	if ops == nil {
		ops = &PluginOps{}
	}
	sess := NewSession(stream)
	sess.SetPeer(&pluginPeer{ops: ops})
	if ops.PushEvent != nil {
		sess.SetPushHandler(ops.PushEvent)
	}
	sess.Start()
	manifestJSON, err := json.Marshal(mf)
	if err != nil {
		sess.Close()
		return nil
	}
	if err := sendFrame(stream, &Frame{Id: f.Id, Reply: true, Kind: KindRegister, Manifest: manifestJSON}); err != nil {
		sess.Close()
		return nil
	}
	<-sess.Done()
	return nil
}

// ServeStream 是框架入站的传输无关握手:在既有 Stream 上收 Register(manifest+token),
// 校验、注册、回执后驱动会话直至流关闭;断流自动 Remove。
func ServeStream(ctx context.Context, rh RemoteHost, stream Stream, token string, logf func(string, ...any)) error {
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	f, err := recvFrame(stream)
	if err != nil {
		return err
	}
	if f.Kind != KindRegister {
		_ = streamReplyErr(stream, f.Id, "remote: first frame must be Register")
		return nil
	}
	if token != "" && f.Token != token {
		_ = streamReplyErr(stream, f.Id, "remote: token rejected")
		return nil
	}
	var mf contract.Manifest
	if err := json.Unmarshal(f.Manifest, &mf); err != nil {
		_ = streamReplyErr(stream, f.Id, fmt.Sprintf("remote: bad manifest: %v", err))
		return nil
	}
	if mf.ID == "" {
		_ = streamReplyErr(stream, f.Id, "remote: manifest id required")
		return nil
	}
	if rh.HasPlugin(mf.ID) {
		_ = streamReplyErr(stream, f.Id, fmt.Sprintf("remote: plugin %s already registered", mf.ID))
		return nil
	}
	hp := newHostPeer(rh, mf.ID)
	sess := NewSession(stream)
	sess.SetPeer(hp)
	hp.SetPush(func(e contract.Event) { _ = sess.PushEvent(e) })
	hp.SetHook(func(ctx context.Context, hookID string, data json.RawMessage) (json.RawMessage, error) {
		return sess.Hook(ctx, hookID, data)
	})
	sess.setCleanup(hp.UnsubAll)
	adapter := NewAdapter(sess, mf)
	if err := rh.Register(adapter); err != nil {
		_ = streamReplyErr(stream, f.Id, "remote: "+err.Error())
		sess.Close()
		return nil
	}
	if err := sendFrame(stream, &Frame{Id: f.Id, Reply: true, Kind: KindRegister, Manifest: f.Manifest}); err != nil {
		// 注册已成功但回执发送失败:主动卸载,避免留下一个回不了话的死会话挂在宿主上。
		_, _ = rh.Remove(mf.ID, false)
		sess.Close()
		return nil
	}
	sess.OnClosed(func(err error) {
		if _, rerr := rh.Remove(mf.ID, false); rerr != nil {
			log("remote: unregister %s: %v", mf.ID, rerr)
		} else {
			log("remote: plugin %s unregistered (stream ended: %v)", mf.ID, err)
		}
	})
	log("remote: plugin %s registered (inbound)", mf.ID)
	sess.Start()
	<-sess.Done()
	return nil
}
