package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/ejfkdev/egop/contract"
	"github.com/ejfkdev/egop/undo"
	"github.com/tetratelabs/wazero/api"
)

// inst 是单个 wazero module 实例及其串行化状态。guest 实例非并发安全:
// 每个 inst 一把 mu,跨边界入口(函数/工具)持其 mu 执行;宿主注入函数在同一
// mu 内回调(经 instForModule 认回本 inst)。
//
// 实例池(Plugin.insts)解决同 goroutine 嵌套重入死锁:外层调用持 instA,
// 嵌套的宿主→guest 回调经信号量取第二配额+instB(非重入锁 TryLock 跳过持锁者),
// 不再自死锁(2026-09-10 eha 嵌套轮 live 实锤的 egop 层正解)。池大小由清单
// 扩展 egop.pool 声明(缺省 1=旧语义;状态型插件如 messenger/longrun 保持 1)。
type inst struct {
	name   string // wazero module 名(唯一:<plugin>#<idx>g<gen>)
	mod    api.Module
	mu     sync.Mutex
	broken atomic.Bool // 意外打断(ctx 取消/trap):本实例 fail-closed,acquire 时 revive
	// closeOnRelease 让 Close 超时后仍由持锁调用在返回路径清理实例资源;
	// 不另起可能永久等待实例锁的 goroutine。
	closeOnRelease atomic.Bool
	unsubs         undo.Catcher

	netSeq    int64                // 出站流式 body 句柄序号(本 inst mu 下自增)
	netBodies map[string]io.Reader // 句柄 → 未读完的响应 body
	kvTx      *kvUpdate            // 当前模块的 KV 事务(本 inst mu 下访问)
}

// kvUpdate 把 backend 的同步 Update 回调横跨多次 guest ABI 调用。宿主 goroutine
// 持 backend 锁等待 done；当前 inst 的普通 kv_* import 改用 view，不再次取锁。
type kvUpdate struct {
	view     contract.KeyValue
	done     chan struct{}
	finished chan struct{}
	stopOnce sync.Once
	err      error // 只在 finished 关闭后读取
}

func (tx *kvUpdate) stop() { tx.stopOnce.Do(func() { close(tx.done) }) }

// beginKVUpdate 进入 backend Update 并等待其事务视图就绪(调用方持 inst.mu)。
// 旧 backend 没有扩展时明确报错；绝不退回无锁 Get/Put 序列。
func (i *inst) beginKVUpdate(kv contract.KeyValue) (contract.KeyValue, error) {
	if i.kvTx != nil {
		return nil, fmt.Errorf("kv update already active")
	}
	updater, ok := kv.(interface {
		Update(func(contract.KeyValue)) error
	})
	if !ok {
		return nil, fmt.Errorf("kv update not supported by backend")
	}
	tx := &kvUpdate{done: make(chan struct{}), finished: make(chan struct{})}
	ready := make(chan contract.KeyValue, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				tx.err = fmt.Errorf("kv update backend panic: %v", r)
			}
			close(tx.finished)
		}()
		tx.err = updater.Update(func(view contract.KeyValue) {
			tx.view = view
			ready <- view
			<-tx.done
		})
	}()
	select {
	case view := <-ready:
		i.kvTx = tx
		return view, nil
	case <-tx.finished:
		tx.stop() // 防御异步/异常 backend:回调若迟到也不得遗留 goroutine。
		if tx.err != nil {
			return nil, tx.err
		}
		return nil, fmt.Errorf("kv update backend returned before callback")
	}
}

// endKVUpdate 幂等释放当前事务并等待 backend 退出(调用方持 inst.mu)。
func (i *inst) endKVUpdate() error {
	tx := i.kvTx
	if tx == nil {
		return nil
	}
	i.kvTx = nil
	tx.stop()
	<-tx.finished
	return tx.err
}

func newInst(name string) *inst {
	return &inst{name: name, netBodies: map[string]io.Reader{}}
}

// netAlloc 登记一条流式响应 body,返回字符串句柄(宿主注入函数内、持本 inst mu 调用)。
func (i *inst) netAlloc(r io.Reader) string {
	i.netSeq++
	key := fmt.Sprintf("%d", i.netSeq)
	i.netBodies[key] = r
	return key
}

// netGet 取句柄对应的 body 读端;未知/已关返回 nil。
func (i *inst) netGet(handle string) io.Reader {
	if i.netBodies == nil {
		return nil
	}
	return i.netBodies[handle]
}

// netClose 关闭并遗忘一条 body 句柄(幂等;body 若未实现 io.Closer 仅遗忘)。
func (i *inst) netClose(handle string) {
	if b, ok := i.netBodies[handle]; ok {
		if c, ok := b.(io.Closer); ok {
			_ = c.Close()
		}
		delete(i.netBodies, handle)
	}
}

func (i *inst) netCloseAll() {
	for h := range i.netBodies {
		i.netClose(h)
	}
}

// callCore 打参进 guest 并执行导出(要求已持 i.mu),返回原始 results。
// ctx 取消/超时 → 看门狗经 CloseWithExitCode 打断 guest 并把本实例置 broken
// (下次调用 revive)——挂死的 guest 不永挂调用方/投递方。
func (i *inst) callCore(p *Plugin, ctx context.Context, fname string, args ...string) ([]uint64, error) {
	// guest trap/取消/遗漏 end 都不能把 backend 路径锁带进下一次调用。
	defer i.endKVUpdate()
	if i.broken.Load() {
		return nil, fmt.Errorf("wasm plugin %s: instance %s closed", p.name, i.name)
	}
	fn := i.mod.ExportedFunction(fname)
	if fn == nil {
		return nil, fmt.Errorf("wasm plugin %s: export %q missing", p.name, fname)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	defer close(done)
	var finished atomic.Bool
	if ctx.Done() != nil { // 可取消才装看门狗;Background 不浪费每调 goroutine
		go func() {
			select {
			case <-done:
			case <-ctx.Done():
				if finished.Load() {
					return // 调用已完成:迟到的取消不再误打断/误置 broken
				}
				_ = i.mod.CloseWithExitCode(ctx, 255)
				i.broken.Store(true)
			}
		}()
	}
	params := make([]uint64, 0, len(args)*2)
	for _, a := range args {
		ptr, err := allocWriteGuest(ctx, i.mod, []byte(a))
		if err != nil {
			// 非 ctx 取消的分配失败=实例已死(module closed 等)——置 broken,
			// 下次调用 revive。此前只有看门狗中断路径置 broken:guest 自发
			// exit/trap 后实例永不自愈,插件僵死到进程重启(生产事故实锤)。
			if ctx.Err() == nil {
				i.broken.Store(true)
			}
			return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
		}
		params = append(params, uint64(ptr), uint64(len(a)))
	}
	results, err := fn.Call(ctx, params...)
	finished.Store(true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("wasm plugin %s: %s: interrupted: %w", p.name, fname, ctx.Err())
		}
		// guest trap/自发退出(exit_code 等)=实例不可信,置 broken 待 revive
		// (fail-closed:当次调用如实报错,绝不带病复用)。
		i.broken.Store(true)
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
	}
	return results, nil
}

// callVoid 在本 inst 上调用无返回值的 guest 导出(egop_on_event;要求已持 i.mu)。
// 同款看门狗:挂死的投递处理器按时限打断。
func (i *inst) callVoid(p *Plugin, ctx context.Context, fname string, args ...string) error {
	_, err := i.callCore(p, ctx, fname, args...)
	return err
}

// callExport 在本 inst 上调用返回信封的 guest 导出(要求已持 i.mu)。
func (i *inst) callExport(p *Plugin, ctx context.Context, fname string, args ...string) (json.RawMessage, error) {
	results, err := i.callCore(p, ctx, fname, args...)
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("wasm plugin %s: %s: bad result arity", p.name, fname)
	}
	ptr, ln := unpack(results[0])
	s, err := readGuestString(i.mod.Memory(), ptr, ln)
	if err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: %w", p.name, fname, err)
	}
	var env envelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, fmt.Errorf("wasm plugin %s: %s: bad result envelope: %w", p.name, fname, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("wasm plugin %s: %s: %s", p.name, fname, env.Error)
	}
	if env.ResultB64 != "" {
		data, err := base64.StdEncoding.DecodeString(env.ResultB64)
		if err != nil {
			return nil, fmt.Errorf("wasm plugin %s: %s: bad result_b64: %w", p.name, fname, err)
		}
		return data, nil
	}
	return env.Result, nil
}
