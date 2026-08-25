;; 配置读回(ConfigProvider)参考夹具:导出 egop_get_config 返回当前生效配置的裸 JSON
;; (ptr,len),供宿主 EffectiveConfig 优先读。编译产物 config.egop.wasm 入库;
;;   wat2wasm config.wat -o config.egop.wasm
(module
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
    i32.const 4096
    i64.extend_i32_u
    i64.const 112
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_get_config") (result i64)
    i32.const 4208
    i64.extend_i32_u
    i64.const 46
    i64.const 32
    i64.shl
    i64.or)
  (data (i32.const 4096) "{\"id\":\"wasm.cfg\",\"name\":\"Cfg\",\"version\":\"1.0.0\",\"provides\":{\"config\":[{\"key\":\"api_url\"},{\"key\":\"max_results\"}]}}")
  (data (i32.const 4208) "{\"api_url\":\"https://default\",\"max_results\":10}"))