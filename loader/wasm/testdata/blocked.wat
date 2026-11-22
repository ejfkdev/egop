;; Test-only non-cooperative guest: reports entry, then spins until forcibly closed.
(module
  (import "egop" "log" (func $log (param i32 i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (global $heap (mut i32) (i32.const 4096))
  (func (export "egop_host_alloc") (param $size i32) (result i32)
    (local $p i32)
    global.get $heap
    local.set $p
    local.get $p
    local.get $size
    i32.add
    global.set $heap
    local.get $p)
  (func (export "egop_call") (param i32 i32 i32 i32) (result i64)
    i32.const 1024
    i32.const 4
    i32.const 1028
    i32.const 7
    call $log
    drop
    (loop $forever
      br $forever)
    unreachable)
  (data (i32.const 1024) "info")
  (data (i32.const 1028) "started"))
