// Package undo 是 effect 级联的最小实装（cordis ctx.effect 的 Go 化）。
// 一切资源获取返回撤销函数,统一进 Catcher——Close 时 LIFO 反序、逐条错误
// 隔离（一条坏不拖累其余）、聚合报告、重复关闭安全。
package undo

import (
	"errors"
	"sync"
)

// Catcher 收集撤销函数（注册序;关闭时反序执行）。
type Catcher struct {
	mu   sync.Mutex
	done bool
	fns  []func() error
}

// Add 追加撤销函数（返回错误版,用于 Close 聚合）。
func (c *Catcher) Add(fn func() error) {
	if fn == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.fns = append(c.fns, fn)
	}
}

// Defer 追加无错撤销（惰性包裹,便利面）。
func (c *Catcher) Defer(fn func()) {
	c.Add(func() error { fn(); return nil })
}

// Close 反序清退全部撤销函数:单条失败隔离,errors.Join 聚合;幂等。
func (c *Catcher) Close() error {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil
	}
	c.done = true
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()

	var errs []error
	for i := len(fns) - 1; i >= 0; i-- {
		if err := fns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
