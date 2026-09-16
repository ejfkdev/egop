// 全局文件系统能力面示例:fs.read / fs.write 分向门控(先说后做)——装配层注入
// contract.FS 后端(范围/沙箱策略由实现决定),插件按声明拿到裁剪后的视图:
// 只读声明者写被拒、只写声明者读被拒、未声明者拿不到面。
// 与 storage.persist(插件专属隔离目录)互补:这是宿主文件系统的一个显式受控视图。
// 运行:go run .
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// scopedFS 是装配层注入的全局文件系统后端(内存演示):策略完全由实现自定——
// 这里限定只暴露 "shared/" 前缀,其余路径一律按不存在处理(沙箱在实现侧,
// egop 只管能力门控)。
type scopedFS struct {
	mu    sync.RWMutex
	files map[string][]byte
}

func newScopedFS() *scopedFS {
	return &scopedFS{files: map[string][]byte{
		"shared/hello.txt": []byte("world"),
		"private/secret":   []byte("nope"),
	}}
}

func (f *scopedFS) ReadFile(name string) ([]byte, error) {
	if !strings.HasPrefix(name, "shared/") {
		return nil, errors.New("scoped fs: path outside shared/ not visible")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, ok := f.files[name]
	if !ok {
		return nil, errors.New("scoped fs: not found: " + name)
	}
	return b, nil
}

// ReadDir 同 ReadFile 的作用域纪律:仅 shared/ 内可见(一层)。
func (f *scopedFS) ReadDir(name string) ([]contract.DirEntry, error) {
	if name != "shared" && name != "shared/" && !strings.HasPrefix(name, "shared/") {
		return nil, errors.New("scoped fs: path outside shared/ not visible")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	inner := strings.TrimPrefix(strings.TrimPrefix(name, "shared"), "/")
	sub := "shared"
	if inner != "" {
		sub = "shared/" + inner
	}
	prefix := strings.TrimSuffix(sub, "/") + "/"
	seen := map[string]bool{}
	var out []contract.DirEntry
	for k := range f.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.Index(rest, "/"); i >= 0 {
			if d := rest[:i]; !seen[d] {
				seen[d] = true
				out = append(out, contract.DirEntry{Name: d, IsDir: true})
			}
			continue
		}
		if rest != "" && !seen[rest] {
			seen[rest] = true
			out = append(out, contract.DirEntry{Name: rest})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *scopedFS) WriteFile(name string, data []byte) error {
	if !strings.HasPrefix(name, "shared/") {
		return errors.New("scoped fs: path outside shared/ not writable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = data
	return nil
}

// fsPlugin 是声明了 FS 能力的插件:函数面演示读/写各自的可用地。
type fsPlugin struct {
	meta    contract.Meta
	surface contract.Surface
}

func (p *fsPlugin) Meta() contract.Meta           { return p.meta }
func (p *fsPlugin) SetSurface(s contract.Surface) { p.surface = s }

func (p *fsPlugin) CallFunc(ctx context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	fsys, ok := p.surface.FS()
	if !ok {
		return nil, errors.New("fs face not available")
	}
	switch fname {
	case "read":
		var a struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(input, &a)
		b, err := fsys.ReadFile(a.Name)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"data": string(b)})
	case "write":
		var a struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}
		_ = json.Unmarshal(input, &a)
		if err := fsys.WriteFile(a.Name, []byte(a.Data)); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]bool{"written": true})
	case "dir":
		var a struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(input, &a)
		entries, err := fsys.ReadDir(a.Name)
		if err != nil {
			return nil, err
		}
		return json.Marshal(entries)
	}
	return nil, errors.New("unknown function")
}

func main() {
	ctx := context.Background()
	back := newScopedFS()
	h := host.New[any](host.Options[any]{FS: back, Logf: log.Printf})

	register := func(id string, caps ...string) {
		p := &fsPlugin{meta: contract.Meta{ID: id, Name: id, Version: "1",
			Provides: contract.Provides{
				Capabilities: caps,
				Functions:    []contract.FuncSpec{{Name: "read"}, {Name: "write"}, {Name: "dir"}},
			}}}
		if err := h.Register(p); err != nil {
			log.Fatal(err)
		}
	}
	register("demo.reader", contract.CapFSRead)  // 只读
	register("demo.writer", contract.CapFSWrite) // 只写
	register("demo.blind")                       // 未声明

	call := func(id, fn, in string) (string, error) {
		out, err := h.Call(ctx, id, fn, json.RawMessage(in))
		if err != nil {
			return "", err
		}
		return string(out), nil
	}

	// 只读者:读放行、写被门拒。
	if out, err := call("demo.reader", "read", `{"name":"shared/hello.txt"}`); err != nil {
		log.Fatal(err)
	} else {
		log.Printf("reader read  => %s", out)
	}
	if _, err := call("demo.reader", "write", `{"name":"shared/x","data":"y"}`); err != nil {
		log.Printf("reader write => refused: %v", err)
	}

	// 只写者:写放行、读被门拒。
	if out, err := call("demo.writer", "write", `{"name":"shared/out.txt","data":"from-writer"}`); err != nil {
		log.Fatal(err)
	} else {
		log.Printf("writer write => %s", out)
	}
	if _, err := call("demo.writer", "read", `{"name":"shared/hello.txt"}`); err != nil {
		log.Printf("writer read  => refused: %v", err)
	}

	// 未声明者:拿不到面。
	if _, err := call("demo.blind", "read", `{"name":"shared/hello.txt"}`); err != nil {
		log.Printf("blind read   => refused: %v", err)
	}

	// 实现侧沙箱:即使声明了读,后端策略也挡住 shared/ 之外。
	if _, err := call("demo.reader", "read", `{"name":"private/secret"}`); err != nil {
		log.Printf("reader private => refused by backend policy: %v", err)
	}

	// 列目录(一层,返回 DirEntry):fs.read 同门覆盖 ReadDir——只写者列目录同样被拒。
	if out, err := call("demo.reader", "dir", `{"name":"shared"}`); err != nil {
		log.Fatal(err)
	} else {
		log.Printf("reader dir   => %s", out)
	}
	if _, err := call("demo.writer", "dir", `{"name":"shared"}`); err != nil {
		log.Printf("writer dir   => refused: %v", err)
	}

	_ = h.Close(ctx)
}
