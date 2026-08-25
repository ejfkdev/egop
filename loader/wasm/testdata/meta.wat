;; WASM 插件「目录/配置面」夹具(编译产物 meta.egop.wasm 入库;重新生成:
;;   wat2wasm meta.wat -o meta.egop.wasm)。语义:manifest 声明 plugin.meta +
;;   config.read + config.write;egop_init 依次调用四个宿主注入函数
;;   plugins / get_plugin / get_config / set_config,把各自结果信封 (len<<32|ptr)
;;   存到固定内存槽位 5000/5008/5016/5024,供 wasm_core_test 断言。
(module
  (import "egop" "plugins" (func $plugins (result i64)))
  (import "egop" "get_plugin" (func $get_plugin (param i32 i32) (result i64)))
  (import "egop" "get_config" (func $get_config (param i32 i32 i32 i32) (result i64)))
  (import "egop" "set_config" (func $set_config (param i32 i32 i32 i32 i32 i32) (result i64)))
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
    i64.const 119
    i64.const 32
    i64.shl
    i64.or)

  (func (export "egop_init") (result i64)
    i32.const 5000
    call $plugins
    i64.store
    i32.const 5008
    i32.const 2000
    i32.const 3
    call $get_plugin
    i64.store
    i32.const 5016
    i32.const 2000
    i32.const 3
    i32.const 2010
    i32.const 3
    call $get_config
    i64.store
    i32.const 5024
    i32.const 2000
    i32.const 3
    i32.const 2010
    i32.const 3
    i32.const 2020
    i32.const 5
    call $set_config
    i64.store
    i32.const 3200
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)

  (data (i32.const 1024) "{\"id\":\"wasm.meta\",\"name\":\"Meta\",\"version\":\"1\",\"provides\":{\"capabilities\":[\"plugin.meta\",\"config.read\",\"config.write\"]}}")
  (data (i32.const 2000) "svc")
  (data (i32.const 2010) "key")
  (data (i32.const 2020) "\"new\"")
  (data (i32.const 3200) "{\"ok\":true}"))