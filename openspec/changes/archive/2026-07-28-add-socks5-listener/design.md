## Context

The proxy today has a single inbound adapter (`internal/in/http/`) serving both
the PAC and the HTTP/CONNECT forwarder, both of which cascade to the configured
upstream via `dialUpstream` + `tunnel`. These two functions (~110 lines
including `dialProxy`, `tlsDial`, and the `bufferedConn` edge-case wrapper) are
the reusable core of the upstream cascade: "establish a tunneled TCP connection
to `host:port` through the configured HTTPS-CONNECT upstream."

The new SOCKS5 adapter needs exactly this capability and nothing more from the
existing code base. The design problem is therefore mostly about *where the
shared cascade code lives* and *how two listeners coordinate their lifecycle*,
not about inventing new upstream machinery.

Constraints inherited from the project:
- Hexagonal layout (`internal/in/` for inbound adapters, `internal/out/` for
  outbound, `internal/port/` for contracts, `internal/app/` for orchestration).
- `app/` orchestrates I/O but does not *perform* raw network I/O — that lives
  in adapters. So the cascade code does not belong in `app/`.
- Atomic-pointer config snapshots, read per-request/connection.
- Errors-only logging; no per-request logging.
- Minimal dependencies (`fsnotify` + `yaml.v3` only).
- TDD: every new function tested first.

Reference: `proposal.md` for motivation; `docs/architecture.md` for the
existing inbound adapter's shape and invariants.

## Goals / Non-Goals

**Goals:**
- Ship a SOCKS5 CONNECT-only inbound adapter that reuses the proven HTTPS
  cascade, so Telegram traffic is indistinguishable from browser traffic to DPI.
- Preserve the HTTP/PAC path byte-for-byte (no behavior change for existing
  users, including those who never configure `socks5`).
- Keep the upstream-cascade logic in exactly one place — no duplication of the
  subtle tunneling/half-close/buffered-conn code.
- Keep the binary dep-free for the SOCKS5 addition (stdlib-only RFC 1928).

**Non-Goals:**
- SOCKS5 auth, BIND, UDP ASSOCIATE (per proposal).
- IPv6 geo / listen-address parity with the long-term IPv6 plan (we will *parse*
  IPv6 target addresses in SOCKS5, but not promote IPv6 anywhere else).
- Per-listener upstream overrides — the single shared upstream is confirmed.
- Dynamic bind/rebind of the SOCKS5 listener on config change (restart required,
  same as the HTTP `listen` field).

## Decisions

### D1: Extract the upstream cascade into `internal/in/cascade/`

**Choice:** Move `dialUpstream`, `dialProxy`, `tunnel`, `tlsDial`, and
`bufferedConn` out of `internal/in/http/` into a new sibling package
`internal/in/cascade/`. Both inbound adapters import it.

**Rationale:** The cascade is shared infrastructure, not HTTP-specific. Today
it lives in `httpserver` purely because that was the only caller. With a second
caller, the options were:

| Option | Verdict |
|---|---|
| **A. Extract to `internal/in/cascade/`** (chosen) | One source of truth; HTTP and SOCKS5 are clean siblings; hexagonal-friendly |
| B. Move to `internal/app/` | Rejected — `app/` orchestrates I/O but doesn't do raw network I/O; cascade does |
| C. Duplicate in `internal/in/socks5/` | Rejected — ~110 lines including subtle `bufferedConn` half-close edge case; duplication is a maintenance hazard |
| D. Export from `httpserver`, have SOCKS5 import it | Rejected — inverted dependency; SOCKS5 would depend on HTTP concepts |

**Migration risk and how it's controlled:** extracting is a mechanical
move (cut the five symbols, paste into `cascade`, fix import paths, have
`httpserver` import them). The existing HTTP test suite
(`server_test.go`, `chain_test.go`, and the build-tagged `realchain_test.go`)
fully exercises this code path and will catch any regression introduced by the
move. The extraction is a separate commit from the SOCKS5 addition so the two
concerns are reviewable independently.

**Naming:** `cascade` captures "establish a connection *cascaded through* the
upstream proxy to a target." The exported entry point is `Dial`:

```go
// Dial establishes a tunneled TCP connection to target (host:port) by dialing
// the configured upstream and issuing a CONNECT. If u.URL is empty, it dials
// the target directly (mirroring the existing direct-mode semantics).
func Dial(u port.Upstream, target string) (net.Conn, error)
```

The existing `bufferedConn` wrapper stays inside `cascade` and is transparent
to callers — `Dial` returns a `net.Conn` that already accounts for any bytes
the response read left in the bufio buffer.

### D2: Two listeners, manual goroutine coordination, no new deps

**Choice:** Coordinate the HTTP and SOCKS5 `Serve` goroutines with channels and
contexts, matching the style already in `wire.go`. Do NOT introduce
`golang.org/x/sync/errgroup`.

**Rationale:** `wire.go` already coordinates five goroutines (PAC regenerator,
geo fetcher, config watcher, error logger, HTTP server) with channels and
contexts. One more listener fits that pattern. `errgroup` would be a net-new
dependency for ~15 lines of saved boilerplate — poor trade against the
project's minimal-deps posture.

**Shape:**

```
                   ┌─ httpSrv.Serve(httpLn)  → httpDone chan
ctx                │
 ├─ cancel ────────┤                         │
 │                 └─ socksSrv.Serve(socksLn) → socksDone chan (if configured)
 │                                           │
 └─ select:                                  
     <-ctx.Done()    → Shutdown both, await both
     <-httpDone      → SOCKS failed? Shutdown SOCKS too
     <-socksDone     → HTTP failed? Shutdown HTTP too
```

"Either listener failing unexpectedly shuts down the other" mirrors how a
single failing goroutine currently aborts `Run`. The HTTP listener remains
the primary (it serves the PAC, which is the project's headline feature);
if SOCKS5 is the one that fails, we still tear down the whole process rather
than silently running half-broken.

### D3: Opt-in via empty `socks5.listen`, not a separate enable flag

**Choice:** The SOCKS5 listener starts if and only if `socks5.listen` is
non-empty. No separate `socks5.enabled` boolean.

**Rationale:**

| Option | Verdict |
|---|---|
| **A. `socks5.listen == ""` means disabled** (chosen) | Simplest; mirrors `upstream.url == ""` semantics; one knob |
| B. Separate `socks5.enabled: true/false` | Rejected — two knobs to keep consistent; users will set one and forget the other |

This means the SOCKS5 struct is just:

```go
type SOCKS5 struct {
    Listen string `yaml:"listen"` // empty = disabled
}
```

Three config states, all meaningful:
- `socks5:` block absent → `Listen == ""` → disabled (default; backward compat)
- `socks5.listen: ""` explicitly → disabled (operator opt-out)
- `socks5.listen: "127.0.0.1:1080"` → enabled

Validation: when non-empty, `Listen` MUST parse as `host:port` (reusing the
same `net.SplitHostPort` check the HTTP `listen` field uses).

### D4: Per-connection upstream read; bind-time listen read

**Choice:** The SOCKS5 listener reads `cfgPtr.Load().Upstream` on every
accepted connection (hot-reload of upstream applies to the next connection),
but reads `socks5.listen` only at startup (restart required to rebind,
identical to the HTTP `listen` field).

**Rationale:** Mirrors the HTTP listener's split: upstream is per-request
read (see `forwardPlain`/`handleConnect`), listen is bind-time. Keeping the
two inbound adapters symmetric avoids a "why does SOCKS5 behave differently
from HTTP" surprise.

### D5: SOCKS5 wire-protocol support matrix (RFC 1928)

**Choice:** Accept only what the proposal mandates; refuse everything else
with the RFC-mandated reply code.

| Phase | Input | Response |
|---|---|---|
| Greeting | Client offers `0x00` among methods | Select `0x00` |
| Greeting | Client does NOT offer `0x00` | `0xFF` (No Acceptable Methods), close |
| Request | CONNECT (`0x01`), IPv4 (`0x01`) | Process; reply `0x00` on success |
| Request | CONNECT (`0x01`), Domain (`0x03`) | Process; reply `0x00` on success |
| Request | CONNECT (`0x01`), IPv6 (`0x04`) | Process; reply `0x00` on success |
| Request | BIND (`0x02`) | Reply `0x07` (Command not supported), close |
| Request | UDP ASSOCIATE (`0x03`) | Reply `0x07` (Command not supported), close |

**Why IPv6 targets are parsed but not promoted:** the cascade doesn't care
about address families — it hands a `host:port` string to the upstream's
CONNECT, which does its own resolution. Refusing IPv6 here would be an
arbitrary limitation with no benefit. This does NOT contradict the project's
"IPv6 geo is deferred" stance: geo is about RU-range matching, an unrelated
code path. SOCKS5 just forwards whatever target string the client sent,
formatted as `[ipv6]:port` per RFC 3986.

**Reply BND.ADDR/BND.PORT on success:** `0.0.0.0:0`. We don't bind anything
locally (we tunnel through the upstream); RFC 1928 allows this and every
tested SOCKS5 client ignores it. Anything more accurate would require
`getsockname` gymnastics with no observable client benefit.

**Error reply mapping:** all `cascade.Dial` failures (upstream unreachable,
CONNECT rejected, config not published) → reply code `0x01` (general SOCKS
server failure) and log. We do not try to distinguish `0x04`/`0x05` because
the upstream abstracts the real network condition away from us; reporting
"general failure" is honest.

### D6: Separate pure-parse layer for testability

**Choice:** Structure the SOCKS5 package as a thin connection-handling shell
over pure framing functions:

```go
// Pure I/O over io.Reader/io.Writer — trivially testable with bytes.Buffer.
func negotiateMethod(r io.Reader, w io.Writer) error
func readRequest(r io.Reader) (cmd byte, host string, port uint16, err error)
func writeReply(w io.Writer, code byte) error

// The shell: net.Listener.Accept loop, deadlines, cascade.Dial, tunnel.
func Serve(ctx context.Context, ln net.Listener, cfgPtr *atomic.Pointer[port.Config])
```

**Rationale:** the project's TDD convention demands tests-first. Pure
functions over `io.Reader`/`io.Writer` let us write precise byte-level tests
for the protocol edge cases (malformed greeting, truncated request, wrong
version byte) without spinning up listeners. The end-to-end listener tests
then cover the integration (handshake → cascade → tunnel) using the same
in-process `httptest.Server`-as-fake-upstream pattern as `chain_test.go`.

A handshake read deadline (10s, matching the HTTP server's
`ReadHeaderTimeout`) bounds the greeting+request phase against slowloris-style
hangs. The tunnel phase has NO deadline — Telegram holds sockets open for
long periods, and an arbitrary deadline would sever idle-but-alive
connections.

### D7: No routing layer for SOCKS5

**Choice:** SOCKS5 connections bypass PAC, RU-geo, whitelist, and blacklist
entirely. Whatever `host:port` the client requests is what gets tunneled.

**Rationale:** Telegram decides its own destinations (its DCs); routing
policy is the client's job, not ours. Adding a routing layer would (a) be
more code, (b) have no use case for the stated consumer (Telegram), and (c)
risk silently dropping legitimate Telegram traffic that happens to match a
blacklist entry. The HTTP listener keeps its PAC-and-geo routing because
browsers need it; SOCKS5 is intentionally dumber.

## Risks / Trade-offs

- **[Cascade extraction destabilizes the HTTP path]** → Mitigation: do the
  extraction in its own commit *before* any SOCKS5 code lands. The existing
  HTTP test suite (unit + build-tagged real-proxy) must remain green at
  that commit. If it isn't, the extraction is wrong and gets fixed before
  SOCKS5 work begins.

- **[Manual goroutine coordination in `wire.go` is error-prone]** →
  Mitigation: the shape mirrors the existing five-goroutine coordination
  already in `wire.go`. Add a focused test that starts both listeners,
  cancels the context, and asserts both `Serve` goroutines return within
  the shutdown timeout. Also add a test that one listener failing
  unexpectedly tears down the other.

- **[DPI could still fingerprint the TLS-to-upstream flow once TG traffic
  mixes in]** → Not mitigable in http-proxy-local; belongs to the upstream
  infrastructure. Documented as out-of-scope. The first signal
  will be "Telegram worked yesterday, doesn't today" — at which point the
  cascade is still correct and the problem is elsewhere.

- **[Reply `0x0.0.0.0:0` might confuse some esoteric SOCKS5 client]** →
  Acceptable. Tested against the populations that matter (Telegram desktop,
  Telegram Android, the standard Go SOCKS5 client). If a real client breaks,
  switch to `getsockname` — a localized change in `writeReply`.

- **[`bufferedConn` correctness depends on the bufio.Reader used during
  CONNECT-response read staying attached to the conn]** → Already true in
  the current code; preserved by extraction. The extraction test (existing
  HTTP suite) covers this via the buffered-bytes scenario.

- **[Operator sets `socks5.listen: 0.0.0.0:1080` and exposes an unauth
  open proxy to the LAN]** → Documented in `config.example.yaml` with the
  same `127.0.0.1` recommendation the HTTP section already carries. Not
  technically prevented (matches the HTTP listener's posture); the threat
  model explicitly accepts localhost-only operation and warns against
  exposing.

## Migration Plan

Three commits, in order, each independently green:

1. **Extract `internal/in/cascade/`.** Move the five symbols out of
   `httpserver`, fix imports, run the full HTTP test suite. Zero behavior
   change. Reviewable as a pure refactor.
2. **Add `internal/in/socks5/` + `port.SOCKS5` config field.** New code only;
   the listener is wired but not yet started from `wire.go`. All SOCKS5
   package tests pass; `wire.go` unchanged so the binary behaves identically.
3. **Wire the SOCKS5 listener into `wire.go` + update `config.example.yaml`
   and docs.** Now operators who add a `socks5` block get the listener;
   everyone else sees no change.

**Rollback:** revert commit 3 to disable the feature without removing the
code; revert commits 3+2 to remove the code entirely; revert all three to
return to today's state. Each revert leaves the tree coherent because each
commit is independently green.

**No config migration:** the new `socks5` block is optional and defaults to
disabled. Existing `config.yaml` files load unchanged.

## Alternatives Considered

### A1: Use `github.com/things-go/go-socks5` instead of hand-rolling

**Decision: rejected.** Audited against the live repository (master, July 2026).

What the audit found:

| Claim / property | Reality |
|---|---|
| README "pure Golang" | True for production code — but `go.mod` declares `golang.org/x/net v0.35.0` |
| `golang.org/x/net` usage | Single import in `server_test.go` only (`x/net/proxy`); **zero production imports** — verified by grepping all 16 production `.go` files |
| Other deps | `stretchr/testify` (test-only); transitively `spew`, `difflib`, `yaml.v3` |
| Size | 1655 production LOC across 16 files |
| Cascade fitness | `WithDial(ctx, network, addr) (net.Conn, error)` is sufficient — `addr` is the target `host:port`, plugs into `cascade.Dial` directly |

Why rejected despite fitting:

1. **Cost saving is illusory.** We'd write ~20 lines instead of ~150 — but the
   ~150 we hand-roll is the handshake, which is exactly the surface a
   security-sensitive local proxy should control directly (it's the only
   place DPI could ever fingerprint our inbound). Delegating it to a 1655-LOC
   library we'd then have to audit is a worse trade, not a better one.
2. **Fights the architecture.** Design D6 structures the package as pure
   framing functions over `io.Reader`/`io.Writer` — this is what makes
   byte-level TDD tests trivial. go-socks5's `Serve`/`ListenAndServe` model
   is opaque; we'd lose the pure-fn layer and be forced to test through real
   listeners.
3. **Wrong kind of dependency.** The project's two existing deps
   (`fsnotify`, `yaml.v3`) are load-bearing and not replaceable by stdlib.
   go-socks5's production code uses zero external deps, which proves stdlib
   genuinely suffices — so adding it would be a dep that *is* replaceable,
   the wrong kind to accept.
4. **Pulls in ~1300 LOC we explicitly don't need.** BIND, UDP ASSOCIATE,
   auth, DNS resolver, buffer pool, goroutine pool, middleware chain,
   address rewriter — all features the proposal scopes out. Carrying them
   as dead code in the binary widens the audit surface for nothing.

The one case that would flip this: if we later needed UDP ASSOCIATE
(implementing UDP relay from scratch is genuinely painful). That case is
closed by the proposal — the upstream is TCP-only, so UDP has nowhere to go
even if we implemented it. Reopening it requires an infrastructure change
upstream, not a code change here.

### A2: Implement MTProto instead of SOCKS5

**Decision: rejected.** Closed in the explore session that preceded the
proposal. Recap: MTProto is also TCP-only (no help for UDP calls), its
obfuscation is pointless inside a loopback + TLS-upstream channel (DPI never
sees the localhost hop), and it cannot be cascaded through an HTTP-CONNECT
upstream. SOCKS5 CONNECT-only reuses the existing cascade; MTProto would
require new upstream machinery for zero benefit.

### A3: Use `github.com/sourcegraph/conc` for goroutine coordination

**Decision: rejected.** Audited against the live repository (main, July 2026).

What the audit found:

| Property | Reality |
|---|---|
| Self-described scope | "Designed for short parallel computations"; explicitly identifies "background servers" as the case it's NOT designed for |
| Production deps | Zero (stdlib-only at runtime) |
| Test deps | `stretchr/testify` + 4 transitive |
| Maturity | Pre-1.0; targeted 1.0 in March 2023, still pre-1.0 in July 2026 |

Why rejected:

1. **Categorical use-case mismatch (decisive).** Our two `http.Server.Serve`
   goroutines are lifecycle goroutines that live for the entire process
   lifetime. The library's own README identifies "background servers" as the
   out-of-scope case. This isn't a near-miss — it's exactly the case the
   library disclaims.
2. **Headline feature is inert for us.** conc's main value-add over manual
   coordination is panic propagation with stacktraces. `net/http.Server.Serve`
   recovers per-request panics internally; they never reach the goroutine
   boundary. The feature would do nothing observable.
3. **Consistency.** `wire.go` already coordinates 5 goroutines with channels
   and contexts, no errgroup, no conc. Adding conc for a 6th creates a
   split-style file: half manual, half conc. Worse than either choice applied
   consistently.
4. **Marginal ergonomics.** `conc.WaitGroup.Go()` saves ~10 lines over the
   manual `select` we need for shutdown anyway. Not worth a dep.
5. **Pre-1.0 for 3+ years** signals poor stability fit for a project that
   pins Go and golangci-lint versions for determinism.

The one case that would flip this: if we were rewriting `wire.go` wholesale
to apply structured concurrency to ALL six goroutines, conc would be a
legitimate candidate for the whole file. That refactor is out of scope, has
no motivation beyond ergonomics, and would be risky churn. The middle option
— split style — is the worst of both.

## Open Questions

None blocking. Two minor items to settle during implementation (not before):

- **Package naming:** `cascade` vs. `proxydial` vs. something else. The
  behavior is clear; the name is bikeshed. Decide at commit 1 based on what
  reads best in the import line.
- **Whether to log a one-line "SOCKS5 listener bound to X" at startup.**
  Project convention is errors-only, but a single bind confirmation (not
  per-connection) may be defensible as operational, not request-level.
  Decide when wiring commit 3; if in doubt, stay silent and match the HTTP
  listener (which also doesn't log its bind).
