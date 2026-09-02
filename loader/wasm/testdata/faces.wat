;; 能力面回程参考夹具(编译产物 faces.wasm 入库;重新生成:
;;   wat2wasm faces.wat -o faces.wasm)。egop_call 为 6 参新形状
;; (fname,in,origin 三个字符串对),按 fname 首字节路由:
;;   'O' → 把第三参 origin JSON 包进 {"ok":true,"result":<origin>} 信封回显
;;         (scratch 2200 三段拼接,验证溯源跨 ABI 透传);
;;   'R' → fs_read("hello.txt") 信封原样透传;
;;   'W' → fs_write("out.txt","from-guest") 信封透传;
;;   'N' → net_request(静态 GET https://svc/data)信封透传;
;;   'B' → net_body_read("1")信封透传(每插件首个响应 body 句柄恒为 "1");
;;   'C' → net_body_close("1");
;;   其余 → 静态 {"ok":true}。
(module
  (import "egop" "fs_read" (func $fs_read (param i32 i32) (result i64)))
  (import "egop" "fs_write" (func $fs_write (param i32 i32 i32 i32) (result i64)))
  (import "egop" "net_request" (func $net_request (param i32 i32) (result i64)))
  (import "egop" "net_body_read" (func $net_body_read (param i32 i32) (result i64)))
  (import "egop" "net_body_close" (func $net_body_close (param i32 i32) (result i64)))
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
    i64.const 229
    i64.const 32
    i64.shl
    i64.or)
  (func (export "egop_call") (param $fname i32) (param $flen i32) (param $in i32) (param $inlen i32) (param $optr i32) (param $olen i32) (result i64)
    (local $c i32)
    (local $total i32)
    local.get $flen
    i32.eqz
    if
      i32.const 2100
      i64.extend_i32_u
      i64.const 11
      i64.const 32
      i64.shl
      i64.or
      return
    end
    local.get $fname
    i32.load8_u
    local.set $c
    ;; 'O' = origin 回显:{"ok":true,"result":<origin>}
    local.get $c
    i32.const 79
    i32.eq
    if
      i32.const 2200
      i32.const 2048
      i32.const 20
      memory.copy
      i32.const 2220
      local.get $optr
      local.get $olen
      memory.copy
      i32.const 2220
      local.get $olen
      i32.add
      i32.const 125 ;; '}'
      i32.store8
      local.get $olen
      i32.const 21
      i32.add
      local.set $total
      i32.const 2200
      i64.extend_i32_u
      local.get $total
      i64.extend_i32_u
      i64.const 32
      i64.shl
      i64.or
      return
    end
    ;; 'R' = fs_read("hello.txt")
    local.get $c
    i32.const 82
    i32.eq
    if
      i32.const 2600
      i32.const 9
      call $fs_read
      return
    end
    ;; 'W' = fs_write("out.txt","from-guest")
    local.get $c
    i32.const 87
    i32.eq
    if
      i32.const 2620
      i32.const 7
      i32.const 2640
      i32.const 10
      call $fs_write
      return
    end
    ;; 'N' = net_request(静态 GET)
    local.get $c
    i32.const 78
    i32.eq
    if
      i32.const 2660
      i32.const 41
      call $net_request
      return
    end
    ;; 'B' = net_body_read("1")
    local.get $c
    i32.const 66
    i32.eq
    if
      i32.const 2720
      i32.const 1
      call $net_body_read
      return
    end
    ;; 'C' = net_body_close("1")
    local.get $c
    i32.const 67
    i32.eq
    if
      i32.const 2720
      i32.const 1
      call $net_body_close
      return
    end
    i32.const 2100
    i64.extend_i32_u
    i64.const 11
    i64.const 32
    i64.shl
    i64.or)
  (data (i32.const 1024) "{\"id\":\"wasm.faces\",\"name\":\"Faces\",\"version\":\"1.0.0\",\"provides\":{\"capabilities\":[\"fs.read\",\"fs.write\",\"net.access\"],\"functions\":[{\"name\":\"Origin\"},{\"name\":\"Read\"},{\"name\":\"Write\"},{\"name\":\"Net\"},{\"name\":\"Body\"},{\"name\":\"Close\"}]}}")
  (data (i32.const 2048) "{\"ok\":true,\"result\":")
  (data (i32.const 2100) "{\"ok\":true}")
  (data (i32.const 2600) "hello.txt")
  (data (i32.const 2620) "out.txt")
  (data (i32.const 2640) "from-guest")
  (data (i32.const 2660) "{\"method\":\"GET\",\"url\":\"https://svc/data\"}")
  (data (i32.const 2720) "1"))
