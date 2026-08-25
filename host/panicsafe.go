// 插件不可信 → 机制层 fail-closed:宿主在调用插件代码(函数/配置/工具)的边界处
// 把 panic 归一到 error,绝不炸穿宿主进程。原则(记入 AGENTS.md):机制层负责故障隔离,
// 消费方不重复 recover。
package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// fromPanic 供 defer 使用:把刚发生的 panic 转成 error 写回 errp(语义"插件失败
// 归一到 error,宿主不崩")。
func fromPanic(errp *error, what string) {
	if r := recover(); r != nil {
		*errp = fmt.Errorf("host: %s panicked: %v", what, r)
	}
}

// safeConfig 执行 ConfigProvider.Config() 并把 panic 归一为 nil(读配置失败回退 applied)。
func safeConfig(cp contract.ConfigProvider) (cfg json.RawMessage) {
	defer func() { _ = recover() }()
	return cp.Config()
}

// closeDisposer 执行 Disposer.Close 并把 panic 归一到 error(Close 是插件代码——
// wasm 关停/远程会话收尾/自定义清理可能 panic;fail-closed:记错、不 crash Host.Close)。
func closeDisposer(d contract.Disposer, ctx context.Context, id string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("host: plugin %s Close panicked: %v", id, p)
		}
	}()
	return d.Close(ctx)
}
