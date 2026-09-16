;; 端到端嵌套自调用夹具(编译产物 nested.wasm 入库;重新生成:
;;   wat2wasm nested.wat -o nested.wasm)。语义:egop_call 按 fname 首字节路由——
;;   'd'(deep)=静态 42 信封;'s'(self)=经宿主注入 call 回调**本插件**的 "deep"
;;   (同 goroutine 嵌套重入:池=1 时宿主侧应立即 busy 错误,池≥2 取第二实例)。
;;   这是对 acquire ctx 重入标记跨 wazero 透传的端到端验证(标记不透传=本夹具
;;   的嵌套调用会永挂,测试超时即失败)。
(module
  (import "egop" "call" (func $call (param i32 i32 i32 i32 i32 i32) (result i64)))
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
    i64.const 144
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (result i64)
    (local $c i32)
    local.get $fname
    i32.load8_u
    local.set $c
    ;; 'd' = deep:静态 42 信封
    local.get $c
    i32.const 100
    i32.eq
    if
      i32.const 3200
      i64.extend_i32_u
      i64.const 23
      i64.const 32
      i64.shl
      i64.or
      return
    end
    ;; 其余 = self:call("wasm.nested","deep",{}),宿主信封原样回传
    i32.const 3000
    i32.const 11
    i32.const 3050
    i32.const 4
    i32.const 3100
    i32.const 2
    call $call
    return)
  (data (i32.const 1024) "{\"id\":\"wasm.nested\",\"name\":\"Nested\",\"version\":\"1.0.0\",\"provides\":{\"capabilities\":[\"plugin.call\"],\"functions\":[{\"name\":\"self\"},{\"name\":\"deep\"}]}}")
  (data (i32.const 3000) "wasm.nested")
  (data (i32.const 3050) "deep")
  (data (i32.const 3100) "{}")
  (data (i32.const 3200) "{\"ok\":true,\"result\":42}"))
