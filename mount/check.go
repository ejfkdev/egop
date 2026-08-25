// offline validation exposed for command-line verify flows (装配嘴的离线校验:
// 不注册、只对**存在**的目录逐包装载检查,返回逐包错误——verify 场景坏包=错
// 误,装配场景坏包=告警跳过,两态语义分在调用方)。
package mount

import (
	"context"
	"fmt"
	"os"

	"github.com/ejfkdev/egop/loader/wasm"
)

// CheckDirs 逐目录装载校验插件包(缺省/不存在的目录跳过;扫描有错也递出)。
func CheckDirs(ctx context.Context, dirs []string) []error {
	var errs []error
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		_, errs2 := wasm.ScanDir(ctx, dir, wasm.Options{})
		for _, e := range errs2 {
			errs = append(errs, fmt.Errorf("wasm plugin: %w", e))
		}
	}
	return errs
}
