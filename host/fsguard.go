// 全局文件系统能力门:插件声明 fs.read / fs.write 后拿到的 FS 被 fsGuard 包裹,
// 每个方向的动作按声明分别放行——未声明即报错(先说后做,与 netGuard 协议门同款
// 单点强制)。可见范围/沙箱/路径策略仍由注入的实现决定,本门只管能力语义。
package host

import (
	"fmt"

	"github.com/ejfkdev/egop/contract"
)

// fsGuard 包裹全局文件系统后端,按插件声明分向门控读写。
type fsGuard struct {
	next     contract.FS
	pluginID string
	canRead  bool
	canWrite bool
}

func (g fsGuard) ReadFile(name string) ([]byte, error) {
	if !g.canRead {
		return nil, fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSRead)
	}
	return g.next.ReadFile(name)
}

func (g fsGuard) ReadDir(name string) ([]contract.DirEntry, error) {
	if !g.canRead {
		return nil, fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSRead)
	}
	return g.next.ReadDir(name)
}

// Stat/ReadRange/Append 是 FSEx 的可选扩展:门控按读写方向,底层实现缺席时
// 如实报"不支持"(不静默降级)。范围/沙箱策略仍在注入的实现里。
func (g fsGuard) Stat(name string) (contract.FileStat, error) {
	if !g.canRead {
		return contract.FileStat{}, fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSRead)
	}
	ex, ok := g.next.(contract.FSEx)
	if !ok {
		return contract.FileStat{}, fmt.Errorf("plugin %s: fs stat not supported by this FS", g.pluginID)
	}
	return ex.Stat(name)
}

func (g fsGuard) ReadRange(name string, off, n int64) ([]byte, error) {
	if !g.canRead {
		return nil, fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSRead)
	}
	ex, ok := g.next.(contract.FSEx)
	if !ok {
		return nil, fmt.Errorf("plugin %s: fs range read not supported by this FS", g.pluginID)
	}
	return ex.ReadRange(name, off, n)
}

func (g fsGuard) Append(name string, data []byte) error {
	if !g.canWrite {
		return fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSWrite)
	}
	ex, ok := g.next.(contract.FSEx)
	if !ok {
		return fmt.Errorf("plugin %s: fs append not supported by this FS", g.pluginID)
	}
	return ex.Append(name, data)
}

func (g fsGuard) WriteFile(name string, data []byte) error {
	if !g.canWrite {
		return fmt.Errorf("plugin %s: capability %q not declared", g.pluginID, contract.CapFSWrite)
	}
	return g.next.WriteFile(name, data)
}
