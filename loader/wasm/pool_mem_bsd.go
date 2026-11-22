//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package wasm

import "golang.org/x/sys/unix"

// hostMemoryBytes 主机物理内存(0=未知)。
//
// 不用 stdlib 的 syscall.Sysctl:hw.memsize 是**二进制 int64**,而 Sysctl 会按
// NUL 截断字符串(实测 48GiB 只剩 7 字节 `00 00 00 00 0c 00 00`),解不出来。
// x/sys/unix.SysctlUint64 直接按内核宽度取值(原本就在依赖图里,indirect)。
func hostMemoryBytes() uint64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return v
}
