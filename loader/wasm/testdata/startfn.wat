;; start 函数顺序夹具(编译产物 startfn.wasm 入库;重新生成:
;;   wat2wasm startfn.wat -o startfn.wasm)。模拟 Go wasip1 reactor 插件:
;; 导出 _initialize(置位 inited 标记)——wazero 经 WithStartFunctions 在实例化时
;; 先跑它,之后宿主才读 egop_meta/调 egop_call;egop_call("ready") 回显标记,
;; 证明 start 函数先于任何宿主侧调用执行(否则 Go 插件运行时未初始化)。
(module
  (memory (export "memory") 1)
  (global $heap (mut i32) (i32.const 4096))
  (global $inited (mut i32) (i32.const 0))
  (func (export "_initialize")
    i32.const 1
    global.set $inited)
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
    i64.const 100
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (result i64)
    global.get $inited
    i32.const 1
    i32.eq
    if
      i32.const 2100
      i64.extend_i32_u
      i64.const 22
      i64.const 32
      i64.shl
      i64.or
      return
    end
    i32.const 2140
    i64.extend_i32_u
    i64.const 22
    i64.const 32
    i64.shl
    i64.or)
  (data (i32.const 1024) "{\"id\":\"wasm.startfn\",\"name\":\"StartFn\",\"version\":\"1.0.0\",\"provides\":{\"functions\":[{\"name\":\"ready\"}]}}")
  (data (i32.const 2100) "{\"ok\":true,\"result\":1}")
  (data (i32.const 2140) "{\"ok\":true,\"result\":0}"))
