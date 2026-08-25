;; WASM 插件 ABI 参考夹具(编译产物 demo.wasm 入库;重新生成:
;;   wat2wasm demo.wat -o demo.wasm)。语义:
;;   - egop_meta 返回 (1024,305) 处的 manifest JSON(data 段),
;;   - egop_init 调 egop.get_setting + egop.subscribe_event(JSON 过滤 {"type":"wasm.test.topic"}),
;;   - egop_call 按 fname 长度路由:2="kv"(转发宿主 kv_get 信封) / 4="call"
;;     (调宿主 call("dummy.math","add",{...}) 并原样回传) / 其余=静态 42 信封,
;;   - egop_tool/egop_apply_config 静态 ok 信封,
;;   - egop_on_event 把事件 JSON 拷到 3800(供宿主断言 payload)。
(module
  (import "egop" "get_setting" (func $get_setting (param i32 i32) (result i64)))
  (import "egop" "call" (func $call (param i32 i32 i32 i32 i32 i32) (result i64)))
  (import "egop" "kv_get" (func $kv_get (param i32 i32) (result i64)))
  (import "egop" "subscribe_event" (func $subscribe_event (param i32 i32) (result i64)))
  (import "egop" "on_hook" (func $on_hook (param i32 i32) (result i64)))
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
  (func (export "egop_meta") (result i64)
    i32.const 1024
    i64.extend_i32_u
    i64.const 330
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_init") (result i64)
    i32.const 3050
    i32.const 12
    call $get_setting
    drop
    i32.const 2048
    i32.const 26
    call $subscribe_event
    drop
    i32.const 3500
    i32.const 9
    call $on_hook
    drop
    i32.const 3200
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (result i64)
    local.get $flen
    i32.const 2
    i32.eq
    if
      i32.const 3000
      i32.const 3
      call $kv_get
      return
    end
    local.get $flen
    i32.const 4
    i32.eq
    if
      i32.const 3100
      i32.const 10
      i32.const 3110
      i32.const 3
      i32.const 3120
      i32.const 13
      call $call
      return
    end
    i32.const 3400
    i64.extend_i32_u
    i64.const 23
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_tool") (param $n i32) (param $nl i32) (param $a i32) (param $al i32) (param $c i32) (param $cl i32) (result i64)
    i32.const 3400
    i64.extend_i32_u
    i64.const 23
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_apply_config") (param i32 i32) (result i64)
    i32.const 3200
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_on_event") (param $e i32) (param $el i32)
    i32.const 3800
    local.get $e
    local.get $el
    memory.copy)
  (func (export "egop_on_hook") (param $h i32) (param $hl i32) (param $d i32) (param $dl i32) (result i64)
    i32.const 3510
    i64.extend_i32_u
    i64.const 77
    i64.const 32
    i64.shl
    i64.or)
  (data (i32.const 1024) "{\"id\":\"wasm.demo\",\"name\":\"WASM Demo\",\"version\":\"1.0.0\",\"description\":\"wat fixture\",\"provides\":{\"capabilities\":[\"plugin.call\",\"event.emit\",\"event.listen\",\"tool.provide\",\"storage.kv\"],\"functions\":[{\"name\":\"add\"},{\"name\":\"call\"},{\"name\":\"kv\"}]},\"tools\":[{\"name\":\"answer\",\"description\":\"answer everything\",\"input\":{\"type\":\"object\"}}]}")
  (data (i32.const 2048) "{\"type\":\"wasm.test.topic\"}")
  (data (i32.const 3000) "key")
  (data (i32.const 3050) "test.setting")
  (data (i32.const 3100) "dummy.math")
  (data (i32.const 3110) "add")
  (data (i32.const 3120) "{\"a\":1,\"b\":2}")
  (data (i32.const 3200) "{\"ok\":true}")
  (data (i32.const 3400) "{\"ok\":true,\"result\":42}")
  (data (i32.const 3500) "demo.hook")
  (data (i32.const 3510) "{\"ok\":true,\"result\":{\"block\":true,\"reason\":\"wasm says no\",\"data\":{\"seen\":1}}}")
  (data (i32.const 3600) ""))
