// 插件不可信 → 机制层 fail-closed:宿主在调用插件代码(函数/配置/工具)的边界处
// 把 panic 归一到 error,绝不炸穿宿主进程。原则(记入 AGENTS.md):机制层负责故障隔离,
// 消费方不重复 recover。
package host

import "fmt"

// fromPanic 供 defer 使用:把刚发生的 panic 转成 error 写回 errp(语义"插件失败
// 归一到 error,宿主不崩")。
func fromPanic(errp *error, what string) {
	if r := recover(); r != nil {
		*errp = fmt.Errorf("host: %s panicked: %v", what, r)
	}
}
