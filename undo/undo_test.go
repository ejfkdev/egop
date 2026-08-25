package undo

import (
	"errors"
	"testing"
)

func TestCatcherLIFOOrder(t *testing.T) {
	var order []string
	c := &Catcher{}
	c.Add(func() error { order = append(order, "a"); return nil })
	c.Add(func() error { order = append(order, "b"); return nil })
	c.Add(func() error { order = append(order, "c"); return nil })
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(order) != 3 || order[0] != "c" || order[1] != "b" || order[2] != "a" {
		t.Fatalf("want reverse order c,b,a; got %v", order)
	}
}

func TestCatcherCloseIdempotent(t *testing.T) {
	n := 0
	c := &Catcher{}
	c.Add(func() error { n++; return nil })
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close should be nil: %v", err)
	}
	if n != 1 {
		t.Fatalf("teardown should run exactly once, ran %d", n)
	}
}

func TestCatcherErrorIsolationAndAggregation(t *testing.T) {
	c := &Catcher{}
	boom := errors.New("boom")
	ran := 0
	c.Add(func() error { ran++; return boom })
	c.Add(func() error { ran++; return nil })
	c.Add(func() error { ran++; return errors.New("also bad") })
	err := c.Close()
	if err == nil {
		t.Fatal("Close should aggregate errors")
	}
	if ran != 3 {
		t.Fatalf("all teardowns should run despite errors, ran %d", ran)
	}
	// errors.Join 聚合两个子错误(error 串联文本可资断言)。
	if !errors.Is(err, boom) {
		t.Fatalf("aggregated err should wrap boom: %v", err)
	}
}

func TestCatcherAddAfterCloseIsNoop(t *testing.T) {
	c := &Catcher{}
	c.Close()
	ran := false
	c.Add(func() error { ran = true; return nil })
	if ran {
		t.Fatal("Add after Close should be dropped, not run later")
	}
}

func TestCatcherNilAndDefer(t *testing.T) {
	c := &Catcher{}
	c.Add(nil) // nil 直接忽略,不崩溃
	n := 0
	c.Defer(func() { n++ })
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n != 1 {
		t.Fatalf("Defer wrapper should run once, ran %d", n)
	}
}
