;; 非执行检查的最小合法 ABI 夹具:只导出宿主实际调用的四种签名。
;; 重新生成: wat2wasm inspect.wat -o inspect.wasm
(module
  (memory (export "memory") 1)
  (func (export "egop_host_alloc") (param i32) (result i32)
    i32.const 0)
  (func (export "egop_call") (param i32 i32 i32 i32) (result i64)
    i64.const 0)
  (func (export "egop_tool") (param i32 i32 i32 i32 i32 i32) (result i64)
    i64.const 0)
  (func (export "egop_on_hook") (param i32 i32 i32 i32) (result i64)
    i64.const 0))
