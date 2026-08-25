;; 最小 WASM 插件:无函数的"目录存在"级示例——满足 ABI 的最小面
;; (memory 导出 + egop_host_alloc + egop_meta 回内置清单)。
;; 编译:wat2wasm echo.wat -o echo.egop.wasm(本机 wabt 在
;; /opt/homebrew/Cellar/wabt/1.0.41/bin/),产物放进宿主插件目录即可被
;; ScanDir 发现并注册。
(module
  (memory (export "memory") 1)
  (global $heap (mut i32) (i32.const 4096))
  ;; ABI 必选:宿主往 guest 内存写参数前的分配函数。
  (func (export "egop_host_alloc") (param $size i32) (result i32)
    (local $p i32)
    global.get $heap
    local.set $p
    local.get $p
    local.get $size
    i32.add
    global.set $heap
    local.get $p)
  ;; 无自定义清单段时的清单来源:返回内置 manifest JSON 的 (ptr,len)。
  (func (export "egop_meta") (result i64)
    i32.const 1024
    i64.extend_i32_u
    i64.const 77
    i64.const 32
    i64.shl
    i64.or)
  (data (i32.const 1024) "{\"id\":\"echo\",\"name\":\"Echo\",\"version\":\"1.0.0\",\"description\":\"minimal example\"}"))