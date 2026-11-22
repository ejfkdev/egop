;; _start 一旦执行就 trap;检查器只编译,故本包必须检查成功。
;; 重新生成: wat2wasm inspect_start_trap.wat -o inspect_start_trap.wasm
(module
  (memory (export "memory") 1)
  (func (export "_start")
    unreachable)
  (func (export "egop_host_alloc") (param i32) (result i32)
    i32.const 0))
