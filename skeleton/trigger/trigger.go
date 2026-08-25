// Package trigger 是命名条件触发器契约（框架骨架）。这是独立的事件/条件分发原语，
// 与 contract 的事件总线(Event/EventFilter/MemEvents)是两层；事件总线本体见
// contract + host，本包仅演示"条件触发器"这一种骨架写法。
package trigger

import "context"

type Registry interface {
	Register(id string, cond func(context.Context) bool, fns ...func(context.Context) error) error
	AddCallback(id string, cbID string, fn func(context.Context) error) error
	SetCallback(id string, cbID string, fn func(context.Context) error) error
	RemoveCallback(id string, cbID string) error
	SetCondition(id string, cond func(context.Context) bool) error
	SetCallbacks(id string, fns []func(context.Context) error) error
	SetOrder(fn func(ids []string) []string)
	CallbackIDs(id string) ([]string, bool)
	Condition(id string) (func(context.Context) bool, bool)
	Callbacks(id string) ([]func(context.Context) error, bool)
	Remove(id string)
	IDs() []string
	Len() int
	Check(ctx context.Context) []error
	Fire(ctx context.Context, id string) error
}
