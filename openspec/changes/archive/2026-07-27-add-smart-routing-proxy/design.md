## Context

Greenfield Go project: a localhost HTTP routing proxy that (a) cascades to a configurable upstream proxy, (b) serves a smart PAC at `/proxy.pac` that performs client-side RU-vs-proxy routing, and (c) hot-reloads config and self-fetches its RU geo data. All design decisions below follow the project conventions in `openspec/config.yaml` (TDD mandatory, strict golangci-lint, `go-ultimate` as the canonical Go skill).

The routing logic lives in the PAC (Option A from the explore phase): the browser itself decides per-URL. The Go binary is therefore "dumb" as a router but smart as a PAC generator + geo-data manager. This keeps HTTPS/CONNECT handling simple (the proxy just forwards; no per-connection DNS or geo lookup in Go) and concentrates all routing policy in one auditable artifact.

## Goals / Non-Goals

**Goals:**
- Single, self-contained Go binary: proxy + PAC server + geo fetcher + config watcher.
- Atomic hot-reload of config and PAC; no restart for upstream/list/routing changes.
- Correct routing precedence encoded in the PAC: blacklist > whitelist > local > RU > default-proxy.
- Graceful degradation: failed geo fetch never crashes the process; stale-or-empty cache always yields a servable PAC.
- Layout and conventions per `go-ultimate` (Hexagonal backend service, no `pkg/`, ports/adapters, `wire.go` as composition root).

**Non-Goals:**
- IPv6 geo (deferred to a later change; PAC shape is forward-compatible).
- Mid-flight listener rebinding when `listen` changes (log error, require restart).
- Client-facing auth on the local proxy (localhost-only by design).
- Request-level logging (errors only).
- Auto-update of the binary itself.
- A built-in geo-DB format other than RIPE delegation (MaxMind is a future option, not v1).

## Decisions

### D1. Project layout: Hexagonal backend service (per go-ultimate)

`go-ultimate`'s decision tree routes HTTP servers to the Hexagonal layout. The proxy + PAC server is an HTTP service with one inbound adapter (HTTP) and three outbound concerns (upstream proxy, RIPE fetch, filesystem cache). The project is a single private binary (not importable), so we use the "app private to its executable" form.

```text
http-proxy-local/
├── go.mod                         module github.com/Djarvur/http-proxy-local
├── go.sum
├── .golangci.yml                  Strict config; gate is `golangci-lint run` = 0 findings.
├── mise.toml                      Pins Go + golangci-lint versions; mirrors Makefile tasks.
├── Makefile                       test, lint, run, build targets.
├── .github/
│   └── workflows/
│       ├── pr.yml                 PR sanity: tests + coverage floor + strict lint.
│       └── periodic.yml           Weekly: tests, dependency/vuln checks, latest-linter scan.
├── cmd/
│   └── http-proxy-local/
│       ├── main.go                Thin: flags, signal handling, calls wire.Run().
│       └── wire.go                Composition root: builds Runtime + startable units.
└── internal/
    ├── port/
    │   ├── port.go                App interface + Out ports (Upstream, GeoSource, Cache, Clock).
    │   └── types.go               Config, PACInput, RURanges, errors.
    ├── app/
    │   ├── app.go                 Implements port.App: orchestrates reload + PAC regen.
    │   ├── pac.go                 Renders PAC bytes from Config + RURanges (pure).
    │   ├── ranges.go              Parse RIPE line, sort+merge ranges (pure).
    │   └── runtime.go             Runtime struct: holds atomic pointers, ties app + adapters together.
    ├── in/
    │   └── http/
    │       ├── server.go          http.Server, listener, /proxy.pac + forward handlers.
    │       └── forward.go         Upstream forwarding (plain HTTP + CONNECT), basic auth.
    └── out/
        ├── ripe/adapter.go        Fetches RIPE delegation text over HTTPS.
        ├── fscache/adapter.go     Atomic write/read of ru-cidrs.cache.
        ├── yamlconfig/loader.go   Parse + validate config.yaml.
        └── fsnotify/watcher.go    Debounced fsnotify watcher on config path.
```

**No `.go` files at the module root.** All Go source lives under `cmd/` (entrypoint + wiring) or `internal/` (everything else). `wire.go` lives next to `main.go` in `cmd/http-proxy-local/` so the binary's composition root travels with the binary; both are `package main`. The `Runtime` struct lives in `internal/app/runtime.go` (not loose under `internal/`) as part of the app layer it coordinates.

**Project-level override of go-ultimate default:** `go-ultimate`'s decision tree prescribes a root-level `main.go` for single private binaries and reserves `cmd/<name>/` for importable/multi-binary layouts. This project deliberately uses `cmd/http-proxy-local/main.go` anyway — the operator's explicit preference, captured here as the project-level override permitted by the `engineering-conventions` spec.

**Rationale:** Hexagonal keeps the routing/precedence/merge logic (in `app/`) pure and unit-testable without touching the network or filesystem — essential for TDD. The HTTP layer is a thin adapter; geo source and cache are swappable Out ports (lets us fake RIPE in tests).

**Alternative considered:** flat `main.go + proxy.go + pac.go`. Rejected: harder to test in isolation, no place for the atomic-swap coordination logic, doesn't scale to the cross-cutting reload concerns.

### D2. Live-state coordination: atomic pointers, single writer per concern

Two pieces of mutable shared state: the live `Config` and the live PAC bytes. Both are read on every request and updated by background reloaders. We use `atomic.Pointer[T]` for each, with a single goroutine per writer:

```text
                    ┌─────────────────────────────────────────────────────┐
                    │                    Runtime                          │
                    │                                                    │
   yamlconfig ──►   │  configPtr *atomic.Pointer[Config]   ◄── reads ──┐  │
   (fsnotify)       │                                                    │  │
                    │  pacPtr    *atomic.Pointer[PAC]      ◄── reads ──┤  │
                    │                                                    │  │
                    │  rangesCh  chan RURanges   ──────────────────────┐│  │
                    │  pacRegenCh chan struct{}  ◀─────────────────────┘│  │
                    │                                                    │  │
                    │  HTTP handler reads configPtr + pacPtr per request ◄┘
                    └─────────────────────────────────────────────────────┘
                                              ▲
                                              │
   ripe fetcher ──► app.PacInput(Config, RURanges) ──► app.RenderPAC ──► pacPtr.Store
```

- `configPtr` writer: the config-reload goroutine (single). Validates then stores; on validation failure, logs and keeps old.
- `pacPtr` writer: a dedicated PAC-regenerator goroutine that reads from `pacRegenCh` and re-renders from `configPtr.Load()` + the latest ranges. Single writer guarantees no torn PAC.
- Ranges are passed to the regenerator via a small buffered channel or an `atomic.Pointer[RURanges]`; the regenerator always renders from current config + current ranges, so order doesn't matter.

**Rationale:** atomic pointers give lock-free reads on the hot path (every proxied request, every `/proxy.pac` GET). Single-writer-per-pointer sidesteps the "two reloaders race to regenerate the PAC" problem.

**Alternative considered:** `sync.RWMutex` around a single struct. Rejected — read path is ultra-hot (every request), and RWMutex has writer-priority semantics that can stall readers under frequent reloads.

### D3. PAC shape: ES3-subset, binary-search over merged uint32 ranges

The PAC is rendered from a template with three injected regions: `LOCAL_PROXY` string, whitelist/blacklist matchers, `RU_RANGES` array. The matchers use `shExpMatch` (suffix) and `isInNet`/numeric range (CIDR). The RU lookup is a binary search:

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

Strict ES3 subset: `var`/`function` only, no arrow/template-literal/`const`/`let`. The template lives as a Go string constant in `internal/app/pac.go`; rendering injects config-derived values and the merged ranges. The ranges are pre-sorted and disjoint (D4), so the binary search is correct by construction.

**Rationale:** ~6000 raw RU prefixes collapse to ~3000–4000 merged ranges; binary search turns each `FindProxyForURL` into ~12 comparisons vs 6000 linear `isInNet` calls. PAC stays well under 100KB.

**Alternative considered:** emit `isInNet` per prefix. Rejected: PAC would balloon to ~150KB and every request would do thousands of string comparisons.

### D4. Range merge algorithm: sort by low, single sweep

```
parse each RIPE RU ipv4 line  →  (low, high) uint32 pair
sort by low
sweep: if next.low <= cur.high + 1 → cur.high = max(cur.high, next.high)
       else → emit cur, advance
result: disjoint sorted ranges
```

Pure function in `internal/app/ranges.go`, trivially unit-testable (the spec's "adjacent merged" and "overlapping merged" scenarios are direct test cases).

### D5. Fetcher state machine: blocking first-fetch, dual-interval ticker thereafter

```text
STARTUP
  │
  ├─ cache exists AND fresh?  ── yes ──► load cache, ranges → regen PAC, enter ticker loop
  │
  └─ no ──► BLOCKING fetch (with timeout)
              │
              ├─ success ──► merge + write cache atomically, ranges → regen PAC, enter ticker loop
              ├─ fail + stale cache exists ──► load stale, ranges → regen PAC, log error, enter ticker loop
              └─ fail + no cache ──► empty ranges, regen PAC, log error, enter ticker loop
                                                                       (process does NOT exit)

TICKER LOOP (single goroutine)
  ┌───────────────────────────────────────────────────────┐
  │  wait(nextInterval)  ──► fetch  ──► success?          │
  │                                      │                │
  │                                      ├─ yes ──► merge + write cache, regen PAC,
  │                                      │           nextInterval = refresh_interval
  │                                      │                │
  │                                      └─ no  ──► log error (keep stale/empty cache),
  │                                                 nextInterval = retry_interval
  └───────────────────────────────────────────────────────┘
```

Two intervals govern the loop: `geo.refresh_interval` (default `24h`) is the wait after a successful refresh; `geo.retry_interval` (default `5m`) is the shorter wait after a failed refresh, repeating until success. After a success, the schedule reverts to `refresh_interval`. Hot-reloaded interval values apply to the schedule of subsequent ticks (the in-progress wait is not shortened mid-flight). On any successful refresh, the merged ranges flow into the same `pacRegenCh` path as startup.

**Rationale for fixed retry (not exponential backoff):** RIPE is a stable public endpoint serving this file at scale; a fixed 5m retry is simple, predictable, easy to test, and non-abusive (≤288 attempts/day under continuous failure). Exponential backoff's complexity buys little here.

**Rationale for "no cache + fail → proceed empty" (not exit):** the user explicitly chose graceful degradation over a loud crash. Empty ranges means non-listed hosts route to `PROXY`, which is the safe default for a privacy-sensitive tool (better to over-proxy than to misroute). The failure is logged loudly.

### D6. Hot-reload debounce and atomic-save handling

Editors save via write-temp-then-rename, producing a burst of fsnotify events. The watcher debounces (e.g. 100ms window, last-write-wins) and triggers exactly one reload per burst. Validation failures retain the old config (per spec); no partial state is ever published.

### D7. Cache location and format

Cache at `~/.config/http-proxy-local/ru-cidrs.cache`, co-located with config (single dir to manage/backup). Format: parsed+merged `[low high]` pairs (one per line, two uint32 decimal integers), NOT raw RIPE text — keeps startup load fast (no re-parse) and the file small (~50KB). Atomic write via temp-then-rename. The cache also stores a timestamp (e.g. first line `# updated <unix>`) for the freshness check.

### D8. Engineering conventions wiring

- `.golangci.yml` at repo root enables `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gocyclo`, `gosec`, `revive`, `gocritic` with findings-as-errors. The `Makefile` and `mise.toml` `lint` targets are the gate.
- `mise.toml` pins the Go toolchain and `golangci-lint` versions, and mirrors the Makefile tasks (`build`, `test`, `lint`, `run`). This guarantees every contributor (and CI) runs against identical tool versions from a clean clone.
- TDD: every change below pairs a failing-test task with its implementation task (see tasks.md).
- CI (`pr.yml`, `periodic.yml`) enforces the gate on every PR and keeps the project honest over time (tests, coverage floor, strict lint on PRs; tests + dependency/vuln checks + latest-linter scan weekly).
- All Go architecture questions route through `go-ultimate` (per `engineering-conventions` spec); the Hexagonal layout in D1 is its prescribed backend-service layout.

## Risks / Trade-offs

- **[RIPE format drift]** → Parse defensively (skip unparseable lines with a warning counter); if the parse yields zero RU ranges from otherwise-valid-looking input, treat as fetch failure (don't silently serve empty PAC when the source genuinely broke). Log includes lines-skipped count.
- **[PAC engine quirks across browsers]** → Stick to the documented ES3 subset; avoid engine-specific extensions. Test the rendered PAC against a JS interpreter in CI (e.g. embed a tiny PAC-engine harness, or use `node` with a `FindProxyForURL` shim) to catch syntax regressions.
- **[Stale cache served indefinitely on persistent fetch failure]** → Acceptable per spec (graceful degradation); mitigated by loud logging on each failed fetch so the operator notices. A future change could add a max-stale-age guard.
- **[`listen` change requires restart]** → Explicitly logged on every reload that touches `listen`; documented in the spec. The error message names the field and the required action.
- **[Atomic-pointer swap on hot path]** → Standard Go pattern; well-understood. The only subtlety is ensuring the PAC regenerator reads config+ranges atomically enough — solved by reading both into locals before rendering (a slightly stale but internally-consistent PAC is fine).
- **[First-run UX: blocking fetch]** → Chosen by the user. Worst case a few seconds at login on a cold cache. Mitigated by short fetch timeout so it fails fast rather than hanging.

## Migration Plan

Greenfield — no migration. Deployment is: build binary, place config at `~/.config/http-proxy-local/config.yaml`, run. Recommended macOS deployment is a launchd user agent (`~/Library/LaunchAgents/`) that keeps the process alive; out of scope for this change but documented in a future README.

Rollback is `git revert` + rebuild; no persistent state to migrate (the cache is regenerable).

## Open Questions

None blocking. Deferred items (tracked as Non-Goals):
- IPv6 geo support (later change, PAC shape already forward-compatible).
- MaxMind GeoLite2 as an alternative geo source (later change; the `GeoSource` Out port makes this pluggable).
- launchd unit generation (operational, not architectural).
