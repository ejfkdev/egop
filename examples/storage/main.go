// 插件专属持久化示例(contract.Storage 注入):先说后做——声明 storage.persist/
// storage.kv 才能经 Surface.Persist()/KV() 读写专属隔离存储。egop 不实现存储:
// 本例注入自包含的内存后端(按 pluginID 命名空间隔离),生产装配层可用真实文件/
// 数据库/KV 实现同一面。运行:go run .
package main

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/host"
)

// memStorage 是自包含的内存持久化后端:按 pluginID 命名空间隔离 File 与 KV。
type memStorage struct {
	mu    sync.Mutex
	files map[string]map[string][]byte
	kvs   map[string]map[string][]byte
}

func newMemStorage() *memStorage {
	return &memStorage{
		files: map[string]map[string][]byte{},
		kvs:   map[string]map[string][]byte{},
	}
}

func (s *memStorage) File(pluginID string) contract.FileStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files[pluginID] == nil {
		s.files[pluginID] = map[string][]byte{}
	}
	return &memFile{s: s, id: pluginID}
}
func (s *memStorage) KV(pluginID string) contract.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kvs[pluginID] == nil {
		s.kvs[pluginID] = map[string][]byte{}
	}
	return &memKV{s: s, id: pluginID}
}

type memFile struct {
	s  *memStorage
	id string
}

func (f *memFile) Read(name string) ([]byte, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.files[f.id][name], nil
}
func (f *memFile) Write(name string, data []byte) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.files[f.id][name] = data
	return nil
}
func (f *memFile) List() ([]string, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	out := make([]string, 0, len(f.s.files[f.id]))
	for n := range f.s.files[f.id] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

type memKV struct {
	s  *memStorage
	id string
}

func (k *memKV) Get(key string) ([]byte, bool) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	v, ok := k.s.kvs[k.id][key]
	return v, ok
}
func (k *memKV) Put(key string, v []byte) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	k.s.kvs[k.id][key] = v
}
func (k *memKV) Delete(key string) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	delete(k.s.kvs[k.id], key)
}
func (k *memKV) Keys() []string {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	out := make([]string, 0, len(k.s.kvs[k.id]))
	for key := range k.s.kvs[k.id] {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// keeper 声明 storage.persist + storage.kv,经 Surface.Persist()/KV() 读写专属存储。
type keeper struct{ surface contract.Surface }

func (k *keeper) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.keeper", Name: "Keeper", Version: "1",
		Provides: contract.Provides{
			Capabilities: []string{contract.CapPersist, contract.CapKV},
			Functions:    []contract.FuncSpec{{Name: "save"}, {Name: "load"}, {Name: "list"}},
		},
	}
}
func (k *keeper) SetSurface(s contract.Surface) { k.surface = s }
func (k *keeper) CallFunc(_ context.Context, fname string, input json.RawMessage) (json.RawMessage, error) {
	fs, _ := k.surface.Persist()
	kv, _ := k.surface.KV()
	switch fname {
	case "save":
		var v struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(input, &v)
		_ = fs.Write(v.Name, []byte(v.Value)) // 文件面
		kv.Put(v.Name, []byte(v.Value))       // KV 面
		return json.RawMessage(`{"saved":true}`), nil
	case "load":
		var v struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(input, &v)
		fileVal, _ := fs.Read(v.Name)
		kvVal, _ := kv.Get(v.Name)
		return json.Marshal(map[string]string{"file": string(fileVal), "kv": string(kvVal)})
	case "list":
		files, _ := fs.List()
		keys := kv.Keys()
		return json.Marshal(map[string][]string{"files": files, "keys": keys})
	}
	return nil, nil
}

// blind 不声明 storage.persist/kv → Persist/KV 不可用(拿到 false)。
type blind struct{ surface contract.Surface }

func (b *blind) Meta() contract.Meta {
	return contract.Meta{
		ID: "demo.blind", Name: "Blind", Version: "1",
		Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "probe"}}},
	}
}
func (b *blind) SetSurface(s contract.Surface) { b.surface = s }
func (b *blind) CallFunc(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	_, okFile := b.surface.Persist()
	_, okKV := b.surface.KV()
	return json.Marshal(map[string]bool{"persist": okFile, "kv": okKV})
}

func main() {
	ctx := context.Background()
	h := host.New[any](host.Options[any]{Logf: log.Printf, Storage: newMemStorage()})

	if err := h.Register(&keeper{}); err != nil {
		log.Fatal(err)
	}
	if err := h.Register(&blind{}); err != nil {
		log.Fatal(err)
	}

	_, _ = h.Call(ctx, "demo.keeper", "save", json.RawMessage(`{"name":"greet","value":"hi"}`))
	out, _ := h.Call(ctx, "demo.keeper", "load", json.RawMessage(`{"name":"greet"}`))
	log.Printf("keeper.load() = %s", out)
	out, _ = h.Call(ctx, "demo.keeper", "list", json.RawMessage(`{}`))
	log.Printf("keeper.list() = %s", out)

	bout, _ := h.Call(ctx, "demo.blind", "probe", json.RawMessage(`{}`))
	log.Printf("blind.probe() = %s (未声明 → Persist/KV 不可用)", bout)

	_ = h.Close(ctx)
}
