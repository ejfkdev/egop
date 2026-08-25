// 出站网络协议门:插件声明 net.access 后拿到的 Net 被 netGuard 包裹,
// 在转交装配层实现前校验出站目标必须是**网络协议**(http/https/ws/wss 及
// Options.NetSchemes 补充方案),拒绝 file:// 等本地/特殊 scheme——防止插件
// 借用出站网络后端把任意外部 `io.Reader`/`Stream` 接到本地文件系统。
package host

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/ejfkdev/egop/contract"
)

// defaultNetSchemes 是 egop 内置认可的网络协议 scheme(小写)。
var defaultNetSchemes = []string{"http", "https", "ws", "wss"}

// buildNetSchemes 合并内置方案与装配层补充方案为查找集(小写、trim)。
func buildNetSchemes(extra []string) map[string]bool {
	m := make(map[string]bool, len(defaultNetSchemes)+len(extra))
	for _, s := range defaultNetSchemes {
		m[s] = true
	}
	for _, s := range extra {
		if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
			m[s] = true
		}
	}
	return m
}

// checkNetURL 校验出站目标:有 scheme 且在允许集内。空 scheme(裸路径/协议相对)
// 与非网络 scheme(file/data/javascript/...) 一律拒绝。
func checkNetURL(rawurl string, schemes map[string]bool) error {
	u, err := url.Parse(rawurl)
	if err != nil {
		return fmt.Errorf("net: invalid url %q: %w", rawurl, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return fmt.Errorf("net: url %q has no scheme (must be a network protocol)", rawurl)
	}
	if !schemes[scheme] {
		return fmt.Errorf("net: scheme %q not allowed (must be http/https/ws/wss or an allowed extension)", scheme)
	}
	return nil
}

// netGuard 包裹出站网络后端,在每个出站动作前过协议门。
type netGuard struct {
	next    contract.Net
	schemes map[string]bool
}

func (g netGuard) Request(ctx context.Context, req contract.Request) (*contract.Response, error) {
	if err := checkNetURL(req.URL, g.schemes); err != nil {
		return nil, err
	}
	return g.next.Request(ctx, req)
}

func (g netGuard) DialStream(ctx context.Context, url string, headers map[string]string) (contract.Stream, error) {
	if err := checkNetURL(url, g.schemes); err != nil {
		return nil, err
	}
	return g.next.DialStream(ctx, url, headers)
}
