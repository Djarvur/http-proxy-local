## Why

The proxy today ships a smart PAC and a *dumb* HTTP forwarder. Browsers pick up
the PAC from `http://127.0.0.1:8080/proxy.pac` and decide routing client-side
(`DIRECT` vs `PROXY`); the Go binary only generates the PAC and forwards whatever
reaches it. This split is the project's foundational decision (see
`docs/architecture.md` → "The big decision: PAC-decides-routing") and it works
perfectly for browsers.

It breaks down for clients that **ignore the system PAC and expose no way to
inject one** but still accept an HTTP/HTTPS proxy. The motivating example is
the `zcode` CLI: it honours `HTTP_PROXY`/`HTTPS_PROXY` but has no PAC hook, so
every request it makes — including ones to hosts that are reachable directly
(`z.ai`, RU-domestic endpoints, RFC1918 services) — is forced through the
upstream cascade. That is wasteful (latency, upstream bandwidth), sometimes
broken (a host that the upstream cannot reach but that is reachable directly),
and inconsistent with what a PAC-aware client would do.

The clean fix is **a second HTTP listener, on a separate port, that applies the
same routing precedence as the PAC but server-side**. Browsers keep using the
PAC (they decide routing themselves, as today); PAC-blind clients point at the
smart listener and get the routing decisions made for them. The existing dumb
HTTP listener stays byte-for-byte unchanged.

## What Changes

- **Add a smart HTTP inbound listener** as an *extension* of the existing
  `internal/in/http/` package (NOT a new sibling package). The smart listener:
  - Listens on its own bind address (`smart.listen`), separate from the dumb
    HTTP listener's `listen`.
  - Applies the **same routing precedence** as the PAC (blacklist → whitelist →
    local/private → RU geo → default), evaluated server-side per request.
  - Resolves each target hostname through the system resolver to obtain IPs,
    so CIDR rules and the RU-geo rule can match (the PAC gets this for free via
    the browser's `dnsResolve`; the smart listener must do it itself).
  - Emits an **ordered list of dial strategies** per target, then dials them in
    order until one succeeds:
    - `blacklist` match → `[Direct, Upstream]` (direct first, fall back to
      upstream if direct fails — covers the "z.ai briefly unreachable directly"
      case).
    - `whitelist` match → `[Upstream]` (upstream only).
    - local/private network (plain hostname or RFC1918/loopback/link-local IP)
      → `[Direct]`.
    - RU IPv4 (any resolved IP falls in a RU range) → `[Direct]`, **dialing the
      specific RU IP** so the connection lands on the RU endpoint the geo rule
      matched. No upstream fallback for RU (operator can move a host to the
      whitelist if direct is unreliable).
    - default → `[Upstream]`.
  - Reuses the existing `internal/in/cascade/` package (`Dial` + `Tunnel`) for
    both the Direct path (empty `Upstream`) and the Upstream path. No new
    tunneling code.
  - Does **NOT** serve `/proxy.pac` — PAC-blind clients do not fetch it, and
    serving it from the smart port would be misleading (the PAC's `PROXY`
    directive would point at the *dumb* listener, not the smart one).
- **Add a `smart` config block** to `config.yaml`:
  - `smart.listen` — the bind address. Opt-in, exactly mirroring `socks5.listen`:
    the listener starts if and only if this field is non-empty. There is NO
    activating default port; the operator MUST write a value to enable.
    Recommended value `127.0.0.1:8081`, documented in `config.example.yaml`.
  - `smart.dial_timeout` — per-attempt timeout for Direct dials in the blacklist
    fallback path. Default `3s`. Configurable because the right value depends on
    the operator's network (a flaky link needs longer; a fast link wants shorter
    so the upstream fallback kicks in sooner).
  - `smart.dial_attempts` — number of Direct attempts before giving up and
    falling back to upstream (blacklist case). Default `2`. Configurable because
    one attempt may be too eager on a lossy network and three may be too slow.
  - `smart.negative_cache_ttl` — how long to remember a Direct failure for a
    given hostname, so subsequent requests skip the doomed Direct attempt and go
    straight to the fallback. Default `60s`. Configurable because failure
    duration varies (a transient ARP/TCP issue clears in seconds; a DNS
    misconfiguration lasts minutes). Keyed by **hostname** (host-level
    unavailability, not per-port).
- **Run a third listener concurrently** in the composition root (`wire.go`):
  the existing dumb HTTP listener, the existing SOCKS5 listener (when
  configured), and the new smart HTTP listener (when configured). All three
  share the single live `configPtr`, the single `rangesPtr`, and the single
  upstream definition.
- **Hot-reload**:
  - Routing rule changes (`routing.whitelist`, `routing.blacklist`) apply to the
    next request with no restart (per-request read of `configPtr`), exactly as
    they already apply to the PAC regen path.
  - `smart.listen` changes (including adding/removing the `smart` block) require
    a restart, mirroring the existing `listen` and `socks5.listen` posture — the
    listener is not rebound, started, or stopped mid-flight. The reloader logs
    this clearly.
  - `smart.dial_timeout`, `smart.dial_attempts`, `smart.negative_cache_ttl`
    changes apply to the next request (per-request read) — no restart.
- **Parity with the PAC by construction + tests**, not by sharing code. The
  smart listener ships its own `Decide(host, ips, cfg, ranges)` function in
  `internal/app/`, and a property test asserts that for a corpus of hosts the
  server-side decision matches what the PAC would produce. Extracting the PAC
  renderer and the decision function from a single shared model is explicitly
  deferred (see design Alternatives) — it is a larger refactor that does not
  belong in this change.

Out of scope for this change:
- Refactoring `internal/app/pac.go` into a shared routing model (follow-up;
  documented in design A1).
- Applying routing to the SOCKS5 listener (it stays a dumb tunnel for
  Telegram, per its own design D7).
- A smart-listener-level DNS cache. The system resolver already caches; adding
  a second cache on top is redundant and gets correctness wrong (TTLs, negative
  answers, /etc/hosts). See design D4.
- IP-pinning for plain-HTTP (non-CONNECT) forward requests. Plain HTTP is a
  negligible fraction of modern traffic; the complexity of a custom
  `http.Transport.DialContext` that pins a specific IP is not justified. Only
  CONNECT (the overwhelmingly common case) pins the RU IP.
- IPv6 geo. The PAC is already IPv4-only for geo; the smart listener matches
  that stance. IPv6-only hosts fall through to the default `[Upstream]`, same
  as in the PAC.
- Per-listener upstream overrides, TLS on the smart listener, client auth.
  Localhost-only, no-auth, shared upstream — same posture as the dumb HTTP and
  SOCKS5 listeners.

## Capabilities

### New Capabilities

- `smart-http-listener`: A second HTTP inbound listener, on a separate port,
  that applies the PAC-equivalent routing precedence server-side and emits an
  ordered Direct/Upstream strategy list with fallback for the blacklist rule.
  Covers the precedence mapping, DNS resolution via the system resolver,
  RU-IP pinning for CONNECT, the negative cache for failed Direct attempts,
  the opt-in lifecycle (listener only starts when `smart.listen` is set), and
  the deliberate non-serving of `/proxy.pac`.

### Modified Capabilities

- `config-reload`: Add validation and reload semantics for the new `smart`
  config block. Validation SHALL accept a valid `smart.listen` host:port or an
  absent/empty block; reject malformed addresses; reject non-positive
  `dial_timeout`/`negative_cache_ttl` and non-positive `dial_attempts`. Reload
  SHALL detect `smart.listen` changes and `smart` block add/remove, log that a
  restart is required (mirroring `listen` and `socks5.listen`), and continue
  serving on the previously bound address. `smart.dial_timeout`,
  `smart.dial_attempts`, and `smart.negative_cache_ttl` changes apply per-request
  with no restart and no explicit downstream signal.

## Impact

- **New code**:
  - `internal/app/decide.go` — pure `Decide(host, ips, cfg, ranges)` returning
    an ordered `[]Strategy`. No I/O; trivially testable. Plus the matching
    helpers (suffix match, CIDR match, plain-hostname check, private-IP check,
    RU-range lookup — the last reusing the same binary-search approach as the
    PAC, over the same `port.RURanges`).
  - `internal/in/http/smart.go` — the smart handler. Same package as
    `server.go`; reuses `copyHeaders`, `buildTransport`, the CONNECT hijack
    pattern, and `cascade.Dial`/`cascade.Tunnel`. Adds: per-request system DNS
    resolve, `Decide` call, ordered dial with the negative cache and per-attempt
    timeout, RU-IP pinning for CONNECT.
  - `internal/in/http/negcache.go` — a small hostname → expiry map
    (`sync.Map` or a mutex-guarded `map[string]time.Time`) consulted on the
    Direct path of the blacklist fallback. Lazy eviction on read.
- **Modified code**:
  - `internal/port/types.go` — add `Smart` struct (`Listen`, `DialTimeout`,
    `DialAttempts`, `NegativeCacheTTL`) to `Config`.
  - `cmd/http-proxy-local/wire.go` — start the smart listener alongside the
    dumb HTTP and SOCKS5 listeners when configured; coordinate shutdown of all
    three (extends the existing two-listener coordination by one).
  - `internal/app/runtime.go` — extend `ReloadOrchestrator.Reload` to detect
    `smart.listen` changes (and block add/remove) and emit a restart-required
    error of the same shape as the existing `listen`/`socks5.listen` checks.
  - `internal/out/yamlconfig/loader.go` — defaults + validation for the `smart`
    block.
- **Config**: `config.example.yaml` gains a documented `smart:` block; the
  example values and the "empty = disabled" rule are called out.
- **Docs**: `README.md` gains a section on using the smart listener for
  PAC-blind clients (zcode); `docs/architecture.md` notes the third inbound
  adapter, the deliberate server-side-vs-PAC parity, and the
  fallback-only-for-blacklist asymmetry.
- **No new dependencies**: DNS resolution uses `net.LookupIP` (or
  `net.Resolver.LookupIPAddr`) from the standard library; the negative cache is
  a stdlib-only map; the cascade is reused as-is.
- **Threat model**: unchanged. The smart listener is localhost-only with no
  auth, exactly like the dumb HTTP and SOCKS5 listeners. The new surface (DNS
  resolution done by the proxy) is a privacy shift ONLY for the smart listener:
  the proxy now resolves names for PAC-blind clients that previously resolved
  them at the upstream. This is inherent to server-side routing and is
  documented; the dumb HTTP listener (and its browser clients) is unaffected.
- **Backward compatibility**: full. Operators who do not add a `smart` block
  experience zero behavior change — no new listener binds, no new log lines,
  no new config validation failures. The dumb HTTP listener is not modified.
