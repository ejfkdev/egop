package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ejfkdev/egop/contract"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// kvUpdateTestStore 模拟 storageBridge：每次 Surface.KV 返回新对象，Update
// 却按共享后端串行。第一个 Update 会等两个模块都到 begin 门口，保证并发
// 不是测试碰巧串行；普通 Get/Put 若误绕过事务视图会立即在 mu 上自死锁。
type kvUpdateTestStore struct {
	mu            sync.Mutex
	m             map[string][]byte
	updateCalls   int
	txGets        int
	attemptMu     sync.Mutex
	attempts      int
	secondAttempt chan struct{}
}

type kvUpdateTestKV struct {
	store *kvUpdateTestStore
	tx    bool
}

func (k *kvUpdateTestKV) Get(key string) ([]byte, bool) {
	if k.tx {
		k.store.txGets++
		v, ok := k.store.m[key]
		return append([]byte(nil), v...), ok
	}
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	v, ok := k.store.m[key]
	return append([]byte(nil), v...), ok
}

func (k *kvUpdateTestKV) Put(key string, v []byte) {
	if k.tx {
		k.store.m[key] = append([]byte(nil), v...)
		return
	}
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	k.store.m[key] = append([]byte(nil), v...)
}

func (k *kvUpdateTestKV) Delete(key string) {
	if k.tx {
		delete(k.store.m, key)
		return
	}
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	delete(k.store.m, key)
}

func (k *kvUpdateTestKV) Keys() []string {
	if !k.tx {
		k.store.mu.Lock()
		defer k.store.mu.Unlock()
	}
	out := make([]string, 0, len(k.store.m))
	for key := range k.store.m {
		out = append(out, key)
	}
	return out
}

func (k *kvUpdateTestKV) Update(fn func(contract.KeyValue)) error {
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	k.store.updateCalls++
	if k.store.updateCalls == 1 {
		<-k.store.secondAttempt
	}
	fn(&kvUpdateTestKV{store: k.store, tx: true})
	return nil
}

type kvUpdateTestSurface struct {
	contract.Surface
	store  *kvUpdateTestStore
	legacy bool
}

func (s kvUpdateTestSurface) KV() (contract.KeyValue, bool) {
	if s.legacy {
		return legacyKVUpdateTestKV{m: map[string][]byte{}}, true
	}
	return &kvUpdateTestKV{store: s.store}, true
}

// legacyKVUpdateTestKV 故意只有原 contract.KeyValue 四个方法。
type legacyKVUpdateTestKV struct{ m map[string][]byte }

func (k legacyKVUpdateTestKV) Get(key string) ([]byte, bool) { v, ok := k.m[key]; return v, ok }
func (k legacyKVUpdateTestKV) Put(key string, v []byte)      { k.m[key] = v }
func (k legacyKVUpdateTestKV) Delete(key string)             { delete(k.m, key) }
func (k legacyKVUpdateTestKV) Keys() []string {
	out := make([]string, 0, len(k.m))
	for key := range k.m {
		out = append(out, key)
	}
	return out
}

func kvUpdateTestHostOp(name string) hostOp {
	for _, op := range hostOps {
		if op.name == name {
			return op
		}
	}
	panic("missing host op " + name)
}

func newKVUpdateTestModule(t *testing.T, p *Plugin, name string) (*inst, api.Module) {
	t.Helper()
	r := wazero.NewRuntime(context.Background())
	mod, err := r.InstantiateWithConfig(context.Background(),
		[]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00},
		wazero.NewModuleConfig().WithName(name))
	if err != nil {
		t.Fatal(err)
	}
	i := newInst(name)
	i.mod = mod
	p.insts = append(p.insts, i)
	p.byName[name] = i
	t.Cleanup(func() {
		i.mu.Lock()
		_ = i.endKVUpdate()
		i.mu.Unlock()
		_ = r.Close(context.Background())
	})
	return i, mod
}

func newKVUpdateTestPlugin(store *kvUpdateTestStore, legacy bool) *Plugin {
	p := newPlugin("kv-update-test")
	var surf contract.Surface = kvUpdateTestSurface{store: store, legacy: legacy}
	p.surfacePtr.Store(&surf)
	return p
}

func TestKVUpdateABISerializesModulesAndRoutesOrdinaryKV(t *testing.T) {
	store := &kvUpdateTestStore{m: map[string][]byte{}, secondAttempt: make(chan struct{})}
	p := newKVUpdateTestPlugin(store, false)
	i1, m1 := newKVUpdateTestModule(t, p, "kv-update-1")
	i2, m2 := newKVUpdateTestModule(t, p, "kv-update-2")
	beginOp := kvUpdateTestHostOp(ImportKVUpdateBegin)
	getOp := kvUpdateTestHostOp(ImportKVGet)
	putOp := kvUpdateTestHostOp(ImportKVPut)
	endOp := kvUpdateTestHostOp(ImportKVUpdateEnd)

	start := make(chan struct{})
	errs := make(chan error, 2)
	run := func(i *inst, m api.Module) {
		i.mu.Lock()
		defer i.mu.Unlock()
		defer i.endKVUpdate()
		<-start
		store.attemptMu.Lock()
		store.attempts++
		if store.attempts == 2 {
			close(store.secondAttempt)
		}
		store.attemptMu.Unlock()
		if res := beginOp.run(context.Background(), m, p, nil); res.err != nil {
			errs <- res.err
			return
		}
		res := getOp.run(context.Background(), m, p, []string{"counter"})
		if res.err != nil {
			errs <- res.err
			return
		}
		var got struct {
			Found    bool   `json:"found"`
			ValueB64 string `json:"value_b64"`
		}
		if err := json.Unmarshal(res.raw, &got); err != nil {
			errs <- err
			return
		}
		n := 0
		if got.Found {
			raw, err := base64.StdEncoding.DecodeString(got.ValueB64)
			if err != nil {
				errs <- err
				return
			}
			n, _ = strconv.Atoi(string(raw))
		}
		if res := putOp.run(context.Background(), m, p, []string{"counter", strconv.Itoa(n + 1)}); res.err != nil {
			errs <- res.err
			return
		}
		if res := endOp.run(context.Background(), m, p, nil); res.err != nil {
			errs <- res.err
			return
		}
		errs <- nil
	}
	go run(i1, m1)
	go run(i2, m2)
	close(start)
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("KV Update ABI 未结束；普通 KV 可能绕过了事务视图")
		}
	}
	raw, ok := (&kvUpdateTestKV{store: store}).Get("counter")
	if !ok || string(raw) != "2" {
		t.Fatalf("跨模块事务读改写丢失: counter=%q, ok=%v", raw, ok)
	}
	store.mu.Lock()
	updates, gets := store.updateCalls, store.txGets
	store.mu.Unlock()
	if updates != 2 || gets != 2 {
		t.Fatalf("普通 KV 未全部走 Update 视图: updates=%d txGets=%d", updates, gets)
	}
}

func TestKVUpdateABIBeginRejectsLegacyBackend(t *testing.T) {
	p := newKVUpdateTestPlugin(nil, true)
	i, m := newKVUpdateTestModule(t, p, "kv-update-legacy")
	i.mu.Lock()
	defer i.mu.Unlock()
	res := kvUpdateTestHostOp(ImportKVUpdateBegin).run(context.Background(), m, p, nil)
	if res.err == nil || !strings.Contains(res.err.Error(), "not supported") {
		t.Fatalf("旧 backend 必须明确拒绝 Update: %+v", res)
	}
	res = kvUpdateTestHostOp(ImportKVUpdateEnd).run(context.Background(), m, p, nil)
	if res.err == nil || !strings.Contains(res.err.Error(), "no active transaction") {
		t.Fatalf("失败 begin 后不得留下活动事务: %+v", res)
	}
}

func TestKVUpdateBackendErrorIsExplicit(t *testing.T) {
	// 防止 begin 在 backend 未执行回调就返回时永久等待 ready。
	kv := errorKVUpdateTestKV{}
	i := newInst("error-backend")
	_, err := i.beginKVUpdate(kv)
	if err == nil || !strings.Contains(err.Error(), "backend boom") {
		t.Fatalf("backend 错误未透传: %v", err)
	}
}

type errorKVUpdateTestKV struct{ legacyKVUpdateTestKV }

func (errorKVUpdateTestKV) Update(func(contract.KeyValue)) error {
	return fmt.Errorf("backend boom")
}
