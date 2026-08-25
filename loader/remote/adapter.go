// Adapter 是远程插件的框架侧适配器:一个 contract.Plugin 实现,函数调用/
// 工具/配置全部经会话帧发往对端。注册进宿主后与进程内插件同生命周期。
// 工具彻底去类型:ToolRaw 的 tctx 即线上 JSON(与 ABI 一致),typed 包装留在装配层完成。
package remote

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ejfkdev/egop/contract"
)

// Adapter 实现 contract.Plugin(+FunctionProvider/Configurable)。
type Adapter struct {
	sess     *Session
	manifest contract.Manifest
}

// NewAdapter 构造远程插件适配器。
func NewAdapter(sess *Session, mf contract.Manifest) *Adapter {
	return &Adapter{sess: sess, manifest: mf}
}

// Session 暴露底层会话(装配层接 OnClosed 做失败卸载)。
func (a *Adapter) Session() *Session { return a.sess }

// Manifest 暴露线上清单。
func (a *Adapter) Manifest() contract.Manifest { return a.manifest }

// Meta 实现 contract.Plugin。
func (a *Adapter) Meta() contract.Meta { return a.manifest.Meta }

// CallFunc 实现 contract.FunctionProvider。
func (a *Adapter) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	return a.sess.CallFunc(ctx, fname, input)
}

// ToolSpecs 暴露线上清单的工具声明(装配层做 typed 包装)。
func (a *Adapter) ToolSpecs() []contract.FuncSpec { return a.manifest.Tools }

// ToolRaw 按名返回**无类型工具执行**闭包:tctx 即线上 JSON(ABI 同形;
// 调用方负责把工具上下文序列化为该插件上下文的最小形状)。
// 结果以字符串形态返回供消费方直用。
func (a *Adapter) ToolRaw(name string) (func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error), bool) {
	for _, s := range a.manifest.Tools {
		if s.Name == name {
			return func(ctx context.Context, tctxJSON, args json.RawMessage) (string, error) {
				out, err := a.sess.Tool(ctx, name, args, tctxJSON)
				if err != nil {
					return "", err
				}
				return string(out), nil
			}, true
		}
	}
	return nil, false
}

// applyConfigTimeout 是远程插件应用配置的兜底超时(Configurable 接口本身无 ctx,
// 只能在此设一个上限,避免对端挂起时 host.SetConfig 无限阻塞)。
const applyConfigTimeout = 10 * time.Second

// ApplyConfig 实现 contract.Configurable。
func (a *Adapter) ApplyConfig(cfg json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), applyConfigTimeout)
	defer cancel()
	return a.sess.ApplyConfig(ctx, cfg)
}

// Close 实现 contract.Disposer:宿主关停时清退远程会话(先礼貌 Shutdown 再关底层),
// 把本侧关闭传导为对端 EOF 从而触发卸载——否则 recvLoop 会永久卡在 stream.Recv()。
func (a *Adapter) Close(ctx context.Context) error {
	if ctx.Err() != nil {
		a.sess.Close()
		return nil
	}
	_ = a.sess.Shutdown("host closing")
	a.sess.Close()
	return nil
}
