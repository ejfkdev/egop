//go:build linux

package wasm

import (
	"os"
	"strconv"
	"strings"
)

// hostMemoryBytes 主机物理内存(0=未知)。Linux 读 /proc/meminfo MemTotal。
func hostMemoryBytes() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
