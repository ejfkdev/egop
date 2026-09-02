# egop

[![ci](https://github.com/ejfkdev/egop/actions/workflows/ci.yml/badge.svg)](https://github.com/ejfkdev/egop/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-blue)](#install)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A **content-agnostic plugin-management library for Go** (MIT, module
`github.com/ejfkdev/egop`): plugin register/unregister/hot-replace, static metadata
declaration (eight axes + capability gating), four loading shapes (in-process / WASM
bundle / directory hot-reload / remote channel), and a ctx capability surface. It runs
with zero assembly: `host.New` ships its own in-memory event bus and config events,
`mount.Mount` wires up every external seam in one call. No business types (llm/react/agent
etc.) enter this library — the host is generalized to `Host[C]`, and business capability
is injected at the assembly layer (`Ops`/`OpAliases`/`ToolNames`).

> **Status**: currently **v0.x (pre-1.0)**. The API is not frozen — pin a specific
> commit/tag in `go get`, and check [doc/api.md](doc/api.md) before a minor upgrade.

> 中文文档见 [README.zh.md](README.zh.md)。

## Install

Requires Go 1.26+ (the platform-free core compiles to `js/wasm` and `wasip1`; runtime
capabilities are injected at the assembly layer).

```sh
go get github.com/ejfkdev/egop
```

```go
import (
    "github.com/ejfkdev/egop/contract"
    "github.com/ejfkdev/egop/host"
)
```

## Quick start

In-process, zero assembly — a minimal complete example (copy-paste runnable):

```go
package main

import (
    "context"
    "encoding/json"
    "log"

    "github.com/ejfkdev/egop/contract"
    "github.com/ejfkdev/egop/host"
)

type hello struct{}

func (hello) Meta() contract.Meta {
    return contract.Meta{
        ID: "demo.hello", Name: "Hello", Version: "1",
        Provides: contract.Provides{Functions: []contract.FuncSpec{{Name: "greet"}}},
    }
}
func (hello) CallFunc(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
    return json.RawMessage(`"hi"`), nil
}

func main() {
    ctx := context.Background()
    h := host.New[any](host.Options[any]{Logf: log.Printf}) // in-memory bus/settings/points out of the box
    if err := h.Register(hello{}); err != nil {             // register first
        log.Fatal(err)
    }
    out, _ := h.Call(ctx, "demo.hello", "greet", json.RawMessage(`{}`)) // then call
    log.Printf("greet() = %s", out) // greet() = "hi"
    defer h.Close(ctx)               // reverse-order teardown
}
```

Directory hot-reload + remote plugins — one `mount.Sources` declaration wires up every
external seam:

```go
rt, warns, err := mount.Mount(ctx, h, mount.Sources{
    Dirs:   []string{"./plugins"},     // *.egop.wasm / *.egop.zip(optionally Watch hot-reload)
    Remote: []mount.RemoteSpec{{ID: "x", Addr: "custom://x"}},  // framework dials out
    StreamDial:   func(ctx context.Context, addr string) (remote.Stream, error) { ... },
    StreamAccept: func(ctx context.Context) (remote.Stream, error) { ... }, // plugins dial in
})
for e := range rt.Events() { /* register/replace/remove/failed hot-reload events */ }
defer rt.Close()
```

Runnable examples — every directory has its own README, and the full table lives in
[doc/examples.md](doc/examples.md):

| Theme | Examples |
|---|---|
| In-process | `inproc` (end-to-end), `dependency` (deps / cross-call / batch / slots), `lazy` (lazy + topo-sort), `slots` (slot contract) |
| Capability surface | `capabilities` (gating + Op), `config` (schema validation), `tools` (tool surface), `hooks` (hook dispatch), `events` (pub/sub), `origin` (origin tracing), `controlplane` (introspection) |
| Loading shapes | `wasm` (minimal ABI), `hotreload` (watch), `rawconn` (remote channel), `collab` (plugin metadata / config) |
| Injection seams | `fs` (fs.FS), `storage` (per-plugin persistence), `net` (outbound), `exchange` (envelope table) |

## Documentation

- [doc/contract.md](doc/contract.md) — contract vocabulary & cross-world invariants
- [doc/api.md](doc/api.md) — public API signature reference
- [doc/usage.md](doc/usage.md) — minimal usage snippets by topic
- [doc/examples.md](doc/examples.md) — all runnable examples at a glance
- [doc/decisions.md](doc/decisions.md) — design trade-offs log
- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution guide

> `doc/` and `doc/decisions.md` are currently written in Chinese; the English README is
> the default entry point.

The design follows three cordis mechanisms, mapped onto Go:

| cordis | egop | Notes |
|---|---|---|
| service registry + dispose cascade | `host.Host[C]` Register/Remove(cascade) | cascade teardown + fail-closed |
| schema (static config validation) | `schema` (preset catalog + config validation) | declared ⇒ validated, undeclared ⇒ not delivered |
| ctx injection + effect stack | `contract.Surface` (gated view) + `undo.Catcher` | subscriptions/cleanup on one effect stack |
| loader config + hmr | `mount` + `autoload` | dir add/change/delete = register/replace/remove |

## Package layout

- `contract` — metadata contract (`Meta`/`Manifest`/`SlotSpec`/events/capability words) and
  the `Surface` interface family. **Type source of truth**.
- `host` — the generalized host `Host[C any]`: register/remove/replace, function catalog,
  config schema validation, slot eight-axis diff, Needs dependency, `SurfaceFor`, batch
  load `RegisterMany` (topo-sorted by DepInit).
- `schema` — preset structure catalog + a generic JSON Schema subset validator
  (`Validate`, supports type arrays and `anyOf`).
- `loader/wasm` — loads `*.egop.wasm` (embedded `egop.manifest` custom section, falling back
  to the `egop_meta` export) and `*.egop.zip` (manifest.json + plugin.wasm + assets/;
  plugin.wasm is optional — a zip without it is a **codeless bundle**: pure manifest/assets,
  e.g. UI plugins); extra zip suffixes (brand conventions) inject via `Options.ExtraSuffixes`;
  directory discovery via `ScanDir`.
- `loader/remote` — transport-agnostic remote channel: egop only sends/receives JSON frames
  on an injected `remote.Stream`; the connection is established externally.
- `loader` — the unified host face `loader.HostFace`: assembly components depend only on it.
- `autoload` — hot-reload directory loader: polling + hash + two-phase confirmation.
- `mount` — one-stop assembly: a single `Sources` declaration drives every external seam.
- `skeleton/{loader,config,registry,trigger}` — mechanism skeletons (reference shapes).
- `undo` — the unified effect stack.

## Metadata axes (bidirectional semantics)

`contract.Meta` fields are grouped by direction; `SlotSpec` is the minimal contract for a
"business-module slot", and an implementer is diffed axis-by-axis (no-less-than):

| Axis | Meta field | SlotSpec field | Semantics |
|---|---|---|---|
| emitted points | `provides` | `provides` | framework points the plugin guarantees to emit |
| own hooks | `hooks` | `hooks` | hook points the plugin exposes (modify/observe) |
| event topics | `events` | — | event topics the plugin publishes |
| functions | `functions` | `functions` | callable function catalog |
| capabilities | `capabilities` | `capabilities` | "declare before use": Surface view is trimmed by this |
| config | `config` | `config` | deliverable config fields (validated on delivery) |
| listened points | `listens` | `listens` | framework points the plugin subscribes to |
| prerequisite slots | `requires.deps` (rich) | `needs` (flat) | slot prerequisites are a name list; a plugin's own slot dependency is a rich `Dependency` (slot/kind/version) — same theme, distinct shapes/names |
| tool deps | `needs_tools` | `needs_tools` | tool names the plugin requires |
| tool provide | side effect | `tool.provide` | whether it contributes tools (CapTools) |

**Framework presence check**: every `needs_tools` name must have a provider
(`Options.ToolNames()` or some registered plugin). **Function input/output validation is
on by default**: `Host.Call` JSON-schema-validates `FuncSpec.Input`/`Output` (input rejected
before call, output after), disabled wholesale by `Options.DisableFuncValidation`.

## Loading shapes

```go
h := host.New[MyCtx](host.Options[MyCtx]{...})

// 1. in-process: implement contract.Plugin directly
h.Register(&myPlugin{})

// 2. WASM bundle: .egop.wasm / .egop.zip (directory discovery)
plugs, errs := wasm.ScanDir(ctx, "./plugins", wasm.Options{})

// 3. remote: egop never dials; transport is injected
adapter, sess, err := remote.DialStream(ctx, rh, stream, remote.DialOptions{WantID: "x.id"}) // framework dials out
_ = remote.ServeStream(ctx, rh, stream, "", nil)                                           // plugin dials in
```

In-process and remote plugins share one lifecycle and one gating scheme. The WASM ABI and
remote frame share an isomorphic envelope
`{"ok":bool,"result":any,"result_b64":string,"error":string}` — JSON end to end.

Batch load needs no manual ordering: hand a slice (e.g. `wasm.ScanFS` output) to
`host.RegisterMany`; missing deps / cycles / duplicate ids / unmet slot contracts fail
individually while the rest proceed. For later-arriving deps use `host.RegisterLazy`.

## Hot reload (autoload)

```go
rt, warns, err := mount.Mount(ctx, hf, mount.Sources{
    Dirs:     []string{"./plugins"},
    Watch:    true,             // dir add/change/delete = register/replace/remove
    Remote:   []mount.RemoteSpec{{ID: "x", Addr: "127.0.0.1:7401"}},
    StreamAccept: func(ctx context.Context) (remote.Stream, error) { ... },
})
for e := range rt.Events() { /* register/replace/remove/failed */ }
defer rt.Close()
```

Safety semantics: content-hash + two-phase confirmation (resists partial writes); failed
replace rolls back to the old version; successful replace replays applied config; bad
bundles are isolated warnings; removing a depended-on plugin is fail-closed.

## Metadata: declared is enforced

Besides registration-time axis diffing, `Host` exposes a metadata query surface:
`Dependents(id)` (who depends on it), `CapabilityIndex()` (capability → declarers),
`Functions()` (function catalog snapshot).

## Zero-assembly defaults

`Options` zero value works out of the box: `Events=MemEvents`, `Settings=MapSettings`,
`Points=MemPoints`, `Hooks=MemHooks` — all injectable. Hook dispatch returns a contextual
`HookResult`, subscriptions/hooks are auto-rolled-back per plugin on removal/replace, config
updates broadcast `plugin.config.updated`, `Host.Close` disposes `contract.Disposer`
resources in reverse registration order, `Host.Snapshot()` returns the control-plane JSON.

## ctx capability surface (`contract.Surface`)

At registration the host injects a Surface view trimmed by `Meta.Capabilities`:
`Call` (`plugin.call`), `Plugins`/`GetPlugin` (`plugin.meta`), `GetConfig`/`SetConfig`
(`config.read`/`config.write`), `PublishEvent`/`SubscribeEvent`/`SubscribeEventFilter`
(`event.emit`/`event.listen`), `Persist`/`KV` (`storage.persist`/`storage.kv`), `Exec`
(`exec.cmd`), `Net` (`net.access`), `FS` (`fs.read`/`fs.write` — injected host filesystem
view, gated per direction), and `Op` for injected extensions.

`Host.SurfaceFor(pluginID)` exports the same view — remote plugins' capability callbacks
route through it with identical gating semantics.

## Integrating with your own host (HostFace bridge)

Wrap your existing host in `loader.HostFace` (Register/Replace/Remove/HasPlugin/Plugins/
SetConfig/AppliedConfig/SurfaceFor/Call) and you get wasm dir / hot-reload / remote-channel
loading for free, without touching your host core.

## Cross-platform & browser (I/O injection seams)

The core (`contract`/`host`/`schema`/`undo`/`exchange`/`loader`) has zero os/net dependency
and compiles under `GOOS=js GOARCH=wasm` and `wasip1`. All external I/O goes through
injectable seams: plugin bytes via `wasm.LoadFS`/`ScanFS` + `mount.Sources.FS` /
`autoload.Options.FS`; remote transport via `Sources.StreamDial/StreamAccept`; outbound
network via `host.Options.Net`; persistence via `host.Options.Storage`.

## Commands & maintenance

```sh
make hygiene            # fmt + vet + full tests
go test ./...           # full (loaders include real-wasm e2e)
```

After changing the WASM ABI: update `loader/wasm/testdata/demo.wat` → recompile `demo.wasm`
with wabt's wat2wasm (fixtures are checked in), then run the full suite.

## Boundary

No connection / no built-in encryption on the remote channel (transport + mTLS are
injected); no auto-reconnect; hot-reload uses polling (default 1s); remote channel and wasm
dir loading are desktop-side capabilities — browser consumers use `io/fs.FS`/`LoadFS` bytes
plus in-process registration, and persistence is unavailable unless `Storage` is injected.