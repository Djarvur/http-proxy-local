# Architecture

This document explains *why* the code is shaped the way it is. For *what*
each function does, read the code; for the change history, read
`openspec/archive/`.

## The big decision: PAC-decides-routing

The proxy has two ways it could decide routing:

- **Option A (chosen):** the PAC does all routing decisions client-side.
  The browser evaluates `FindProxyForURL` per request and decides
  `DIRECT` vs `PROXY`. The Go binary is just a PAC generator + a dumb
  forwarder.
- **Option B (rejected):** the proxy does per-connection geo lookups.
  The PAC is trivial ("always send to local proxy") and the Go binary
  resolves hosts and checks IPs.

Option A wins for three reasons:

1. **HTTPS just works.** For HTTPS, the proxy only sees a `CONNECT
   host:443`. Option B would force the proxy to do its own DNS + geo
   lookup on every CONNECT, adding latency and a failure mode. Option A
   offloads that to the browser's `dnsResolve`, which is already in the
   PAC engine.
2. **One source of truth.** Routing policy lives entirely in the PAC. No
   chance for the Go binary and the PAC to disagree.
3. **Cheaper hot path.** The Go binary never does DNS or geo lookups;
   every request is just a forward.

The cost: a larger PAC (~150-200KB with real RU data). That's a one-time
download the browser caches.

## Routing precedence

The PAC evaluates rules top-down, first match wins:

```
FindProxyForURL(url, host)
  │
  1. blacklist match?      ── yes ──►  DIRECT   (absolute veto)
  │  no
  2. whitelist match?      ── yes ──►  PROXY    (overrides local, geo)
  │  no
  3. local/private net?    ── yes ──►  DIRECT
  │  no
  4. RU IPv4?              ── yes ──►  DIRECT
  │  no
  5. default                              ──►  PROXY
```

The asymmetry — blacklist above local, whitelist below blacklist — is
deliberate. Whitelist means "force upstream even for local addresses,"
which is occasionally useful (corporate VPN scenarios). Blacklist means
"this host must NEVER touch the upstream," which must override even the
whitelist to be a meaningful safety valve. If a host is in both lists,
blacklist wins → DIRECT.

## Package layout (Hexagonal, per go-ultimate)

```
cmd/http-proxy-local/          package main
├── main.go                    flags + signal handling, calls Run
└── wire.go                    composition root: builds all adapters, launches goroutines

internal/
├── port/                      boundary types (no logic, no I/O)
│   ├── types.go               Config, Upstream, Geo, Routing, Range, RURanges, PACInput, errors
│   └── port.go                GeoSource, Cache, Clock, ConfigLoader interfaces
├── app/                       pure logic + orchestration
│   ├── pac.go                 RenderPAC — pure function, no I/O
│   ├── ranges.go              ParseRIPELine, MergeRanges, ParseAndMerge — pure
│   ├── runtime.go             RunPACRegenerator, ReloadOrchestrator
│   └── geofetcher.go          GeoFetcher — the dual-interval state machine
├── in/http/                   inbound HTTP adapter
│   ├── server.go              /proxy.pac handler + forwarder + CONNECT + basic auth
│   └── helpers.go             tlsDial, base64 helpers
└── out/                       outbound adapters (implement port.* interfaces)
    ├── fscache/               atomic disk cache (temp-then-rename)
    ├── fsnotify/              debounced config watcher
    ├── ripe/                  HTTPS fetcher for the RIPE delegation list
    └── yamlconfig/            parse + validate config.yaml
```

Key invariants:

- **No `.go` files at the module root.** All source under `cmd/` or
  `internal/`. `wire.go` lives next to `main.go` so the composition
  root travels with the binary.
- **No `pkg/` directory.** `go-ultimate`'s Hexagonal layout puts library
  code under `internal/`; nothing here is meant to be imported
  externally.
- **`port/` is the contract.** It's the only package both `app/` and
  the adapters depend on. Adapters implement `port.GeoSource`,
  `port.Cache`, etc.; `app/` depends on the interfaces, never the
  concrete adapters. This is what makes the orchestration logic
  testable without touching the network or filesystem.

## Live state: atomic pointers

Two pieces of mutable shared state exist:

- `cfgPtr *atomic.Pointer[port.Config]` — the live config snapshot
- `pacPtr *atomic.Pointer[[]byte]` — the live PAC bytes

Both are read on every request (the PAC handler and the forwarder both
call `Load()`), and updated by background goroutines. We use
`atomic.Pointer` rather than a mutex because:

- The read path is ultra-hot (every proxied request, every `/proxy.pac`
  GET). `atomic.Pointer.Load` is lock-free.
- Each pointer has a **single writer goroutine**, sidestepping the
  "two reloaders race to regenerate the PAC" problem:
  - `cfgPtr` writer: the reload orchestrator (invoked by the fsnotify
    watcher goroutine)
  - `pacPtr` writer: a dedicated PAC-regenerator goroutine

The ranges (`*atomic.Pointer[port.RURanges]`) live separately and are
written only by the geo fetcher. The PAC regenerator reads `(cfgPtr,
rangesPtr)` per render; reads them into locals first so a render always
sees an internally-consistent pair (a slightly stale but coherent PAC
is fine).

## Hot reload

```
~/.config/http-proxy-local/config.yaml
   │
   │  fsnotify watcher (50-100ms debounce, write-temp-rename safe)
   ▼
ReloadOrchestrator.Reload()
   │
   ├─ Load(path) → typed Config with defaults applied
   ├─ Validate(c) → check URL scheme, durations, host:port
   │
   ├─ on parse/validate failure: log error, RETAIN old config (no swap)
   │
   └─ on success:
       ├─ cfgPtr.Store(&new)
       ├─ if listen changed: log "restart required" (still swapped so
       │                    per-request reads stay consistent; the
       │                    listener just doesn't rebind mid-flight)
       └─ if PAC-affecting field changed (whitelist/blacklist/listen):
           regen <- struct{}{}  (non-blocking; coalesces if pending)
                              │
                              ▼
              PAC regenerator goroutine
                  ├─ drain pending signals (burst → one render)
                  └─ renderPAC(cfgPtr.Load(), rangesPtr.Load()) → pacPtr.Store
```

The debounce is important: editors save via write-temp-then-rename,
producing a burst of fsnotify events within tens of milliseconds. The
watcher collapses a burst into exactly one reload signal.

## Geo fetcher state machine

Two intervals govern the refresh schedule:

```
SUCCESS path:    refresh ──success──► wait refresh_interval (24h) ──► refresh
                                                       ▲
FAILURE path:    refresh ──fail──► wait retry_interval (5m) ──► refresh
                                  ▲                                │
                                  └───────────── fail ◄────────────┘
                                                │
                                              success ──► back to refresh_interval
```

Startup is a separate state machine:

```
STARTUP
  │
  ├─ cache exists AND fresh (< refresh_interval old)?
  │     ── yes ──► load cache, publish ranges, enter ticker loop
  │
  └─ no ──► BLOCKING fetch (with timeout)
              │
              ├─ success ──► merge + write cache atomically, publish, enter loop
              ├─ fail + stale cache exists ──► load stale, publish, log, enter loop
              └─ fail + no cache ──► publish empty ranges (degraded mode),
                                     log loudly, enter loop
                                     (process does NOT exit)
```

The "no cache + fail → empty ranges, continue" choice is deliberate.
For a privacy-sensitive tool, sending non-listed hosts to the upstream
(`PROXY`) is the safe default — better to over-proxy than to misroute.
The failure is logged loudly on each retry so the operator notices.

## PAC shape: ES3 subset, binary search

PAC engines across browsers are restricted to an ES3-ish subset: `var`
and `function` declarations, no arrow functions, no template literals,
no `const`/`let`. The renderer (`internal/app/pac.go`) enforces this;
the test suite (`pac_test.go`) regex-scans the output to catch
regressions.

The RU lookup uses a binary search over a sorted, disjoint array of
`[low, high]` uint32 pairs:

```js
function inRu(n) {
  var lo = 0, hi = RU_RANGES.length - 1;
  while (lo <= hi) {
    var m = (lo + hi) >> 1, r = RU_RANGES[m];
    if (n < r[0]) hi = m - 1;
    else if (n > r[1]) lo = m + 1;
    else return true;
  }
  return false;
}
```

The ~6000 raw RIPE prefixes are merged into ~3000-4000 disjoint ranges
(`MergeRanges` in `ranges.go`), turning each `FindProxyForURL` into
~12 comparisons instead of 6000 linear `isInNet` calls.

IPv6 is deferred to a future change. The PAC's `ipToInt` rejects IPv6
strings (returns -1, never matches), so IPv6-only hosts fall through to
the default `PROXY` rule.

### Cache headers

The PAC response carries a configurable `Cache-Control` header derived
from `pac.cache_seconds` in the live config (read per request, so
hot-reload applies without restart):

| `pac.cache_seconds` | Emitted header | Meaning |
|---|---|---|
| omitted | `max-age=15, must-revalidate` | Default TTL: browser revalidates within ~15s |
| `0` | `no-cache` (+ `Pragma: no-cache`) | Operator opt-out: revalidate every fetch |
| `N > 0` | `max-age=N, must-revalidate` | Custom TTL |

The `must-revalidate` directive forbids clients from serving a stale PAC
if revalidation fails (e.g. proxy briefly unreachable) — better to drop
the PAC than to route on stale rules. macOS does not re-read the PAC on
a fixed timer; it honors this header, so the TTL directly bounds how
quickly hot-reloaded config reaches the browser.

The `pac.cache_seconds` field is a `*int` in the Config struct (not a
plain int) so the loader can distinguish "field omitted in YAML" (nil →
default applies) from "operator wrote `cache_seconds: 0`" (explicit
opt-out). This is the one field that breaks the "zero means default"
convention used elsewhere; it's documented on the type.

## CI

Two workflows (`.github/workflows/`):

- **`pr.yml`** — every PR + push to main. Runs strict lint (zero
  findings required), tests with the race detector, enforces a
  coverage floor (40%, tunable), and runs the PAC node harness.
  Coverage is uploaded as an artifact.
- **`periodic.yml`** — weekly. Three jobs: tests, `govulncheck` +
  outdated-modules check, and a run against the *latest* golangci-lint
  release (catches new lints the pinned version doesn't know about).
  Failures open an issue via `.github/PERIODIC_FAILURE.md`.

The split (pinned linter on PRs, latest linter weekly) gives
deterministic gating on PRs and forward visibility on the schedule.

## Known limitations

- **IPv6 geo is not implemented.** PAC shape is forward-compatible;
  adding it is a localized change to the renderer + RIPE parser.
- **Geo interval hot-reload is partial.** The fetcher reads
  `Refresh`/`Retry` from its construction-time deps, so changing
  `geo.refresh_interval` mid-flight takes effect only on restart. The
  in-progress wait is never shortened (correct); the next tick uses the
  original value (spec gap). Fix: pass `cfgPtr` to the fetcher and read
  intervals per-tick.
- **No launchd unit generation.** Running as a macOS user agent is the
  intended deployment, but the binary doesn't generate or install the
  plist. Documented as a future addition.
- **CONNECT-via-upstream CONNECT handshake** assumes the upstream
  returns a standard `HTTP/1.1 200` response. Some corporate proxies
  add headers or use non-standard status codes; those would need
  case-by-case handling.
