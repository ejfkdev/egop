;; 死锁复现夹具:egop_init 订阅全事件(filter {}),egop_call 发布事件。
;; 若事件总线同步扇出且 pushEvent 与 guest 调用共用一把互斥锁,发布即自锁。
(module
  (import "egop" "subscribe_event" (func $subscribe_event (param i32 i32) (result i64)))
  (import "egop" "publish_event" (func $publish_event (param i32 i32) (result i64)))
  (memory (export "memory") 1)
  (global $heap (mut i32) (i32.const 8192))
  (func (export "egop_host_alloc") (param $size i32) (result i32)
    (local $p i32)
    global.get $heap
    local.set $p
    local.get $p
    local.get $size
    i32.add
    global.set $heap
    local.get $p)
  (func (export "egop_meta") (result i64)
    i32.const 1024
    i64.extend_i32_u
    i64.const 144
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_init") (result i64)
    ;; 订阅一切事件:filter = {}
    i32.const 2048
    i32.const 2
    call $subscribe_event
    drop
    i32.const 2100
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (result i64)
    ;; 发布一条事件(自己的订阅会命中 → 同步扇出应回调 egop_on_event)
    i32.const 2060
    i32.const 20
    call $publish_event
    drop
    i32.const 2100
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_on_event") (param $e i32) (param $el i32))
  (data (i32.const 1024) "{\"id\":\"wasm.selfpub\",\"name\":\"SelfPub\",\"version\":\"1.0.0\",\"provides\":{\"capabilities\":[\"event.emit\",\"event.listen\"],\"functions\":[{\"name\":\"boom\"}]}}")
  (data (i32.const 2048) "{}")
  (data (i32.const 2060) "{\"type\":\"self.loop\"}")
  (data (i32.const 2100) "{\"ok\":true}"))
