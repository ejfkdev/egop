//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package wasm

// hostMemoryBytes 主机物理内存(0=未知)。未覆盖平台退回 CPU 规模估算。
func hostMemoryBytes() uint64 { return 0 }
