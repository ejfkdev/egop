;; 墙钟夹具:egop_call 读 WASI realtime 钟并返回 "fresh"(秒数 > 2026-01-01)
;; 或 "stale"。wazero RuntimeConfig 缺省是 FakeWallclock(纪元 2022-01-01),
;; guest 内一切时间逻辑(TTL/排程/退避)静默错乱;宿主必须 WithSysWallclock。
;; 本夹具固化该行为(load.go 的墙钟接线有测试兜底,防回退)。
(module
  (import "wasi_snapshot_preview1" "clock_time_get" (func $clock_time_get (param i32 i64 i32) (result i32)))
  (memory (export "memory") 1)
  (global $heap (mut i32) (i32.const 8192))
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
    i64.const 96
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_init") (result i64)
    i32.const 2100
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (result i64)
    (local $sec i64)
    ;; clock_time_get(REALTIME=0, precision=0, out=3000) → u64 纳秒落 3000
    i32.const 0
    i64.const 0
    i32.const 3000
    call $clock_time_get
    drop
    ;; 小端读 u64 纳秒 → 秒
    (i64.load8_u (i32.const 3000))
    (i64.load8_u (i32.const 3001)) i64.const 8  i64.shl i64.or
    (i64.load8_u (i32.const 3002)) i64.const 16 i64.shl i64.or
    (i64.load8_u (i32.const 3003)) i64.const 24 i64.shl i64.or
    (i64.load8_u (i32.const 3004)) i64.const 32 i64.shl i64.or
    (i64.load8_u (i32.const 3005)) i64.const 40 i64.shl i64.or
    (i64.load8_u (i32.const 3006)) i64.const 48 i64.shl i64.or
    (i64.load8_u (i32.const 3007)) i64.const 56 i64.shl i64.or
    i64.const 1000000000
    i64.div_u
    local.set $sec
    ;; sec > 2026-01-01(1767225600)→ fresh
    local.get $sec
    i64.const 1767225600
    i64.gt_u
    if
      i32.const 2200
      i64.extend_i32_u
      i64.const 28
      i64.const 32
      i64.shl
      i64.or
      return
    end
    i32.const 2240
    i64.extend_i32_u
    i64.const 28
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_on_event") (param $e i32) (param $el i32))
  (data (i32.const 1024) "{\"id\":\"wasm.clock\",\"name\":\"Clock\",\"version\":\"1.0.0\",\"provides\":{\"functions\":[{\"name\":\"check\"}]}}")
  (data (i32.const 2100) "{\"ok\":true}")
  (data (i32.const 2200) "{\"ok\":true,\"result\":\"fresh\"}")
  (data (i32.const 2240) "{\"ok\":true,\"result\":\"stale\"}"))
