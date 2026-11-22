;; 缺 egop_host_alloc 的坏 ABI 夹具;模块本身可由 wazero 编译。
;; 重新生成: wat2wasm inspect_no_alloc.wat -o inspect_no_alloc.wasm
(module
  (memory (export "memory") 1)
  (func (export "egop_call") (param i32 i32 i32 i32) (result i64)
    i64.const 0))
