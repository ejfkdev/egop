;; Module-start cancellation fixture: _start spins until the instantiation context
;; is canceled. Rebuild with: wat2wasm startloop.wat -o startloop.wasm
(module
  (memory (export "memory") 1)
  (func (export "_start")
    (loop $forever
      br $forever))
  (func (export "egop_host_alloc") (param i32) (result i32)
    i32.const 0))
