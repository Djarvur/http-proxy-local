## Context

The proxy has two inbound adapters today: a dumb HTTP/PAC listener
(`internal/in/http/`) and an opt-in SOCKS5 listener (`internal/in/socks5/`).
Both share a single upstream cascade (`internal/in/cascade/`) whose entry point
is remarkably suited to this change:

```go
// Dial establishes a tunneled TCP connection to target (host:port) by dialing
// the configured upstream and issuing a CONNECT. If u.URL is empty, it dials
// the target directly.
func Dial(u port.Upstream, target string) (net.Conn, error)
```

The `u.URL == "" → direct dial` branch already exists (it was added for local
testing). This means "Direct vs Upstream" is already a solved problem at the
cascade layer — what is missing is a layer that *chooses* between the two based
on routing rules, the way the PAC chooses for browsers.

The project's foundational decision (`docs/architecture.md` → "The big
decision: PAC-decides-routing") is that routing policy lives in the PAC and the
Go binary is a dumb forwarder. This change does NOT overturn that decision for
browser traffic — it adds a *third* listener, on a separate port, that applies
the same policy server-side for clients that cannot use a PAC. The dumb HTTP
listener is untouched; browser behaviour is byte-for-byte unchanged.

Constraints inherited from the project:
- Hexagonal layout. `app/` holds pure logic + orchestration; adapters do raw
  I/O. So `Decide()` lives in `app/` (pure), and the DNS resolve + ordered dial
  live in the `http` adapter (I/O).
- Atomic-pointer config/ranges snapshots, read per-request.
- Errors-only logging; no per-request success logging.
- Minimal dependencies (`fsnotify` + `yaml.v3` only; this change adds none).
- TDD: every new function tested first.
- The PAC renderer (`internal/app/pac.go`) is ES3-subset JavaScript with a
  regex-scanned test guard; touching it risks regressions and is to be avoided
  unless clearly justified.

Reference: `proposal.md` for motivation; `docs/architecture.md` for the
existing inbound adapter shape and invariants; the `add-socks5-listener`
archive for the opt-in-listener pattern this change mirrors.

## Goals / Non-Goals

**Goals:**
- Ship a smart HTTP listener that applies PAC-equivalent routing precedence
  server-side, so PAC-blind clients (zcode) get the same DIRECT/Upstream
  decisions a browser would make via the PAC.
- Preserve the dumb HTTP/PAC path byte-for-byte (no behaviour change for
  browser users, including operators who never configure `smart`).
- Reuse the existing `cascade.Dial`/`cascade.Tunnel` for both Direct and
  Upstream paths — no new tunneling code.
- Keep the smart listener's routing parity with the PAC verifiable by an
  automated property test.
- Keep the binary dep-free for this addition (stdlib-only DNS, stdlib-only
  negative cache).

**Non-Goals:**
- Refactoring `pac.go` into a shared routing model from which both the PAC and
  `Decide()` are generated. Larger refactor; explicitly deferred (see A1).
- Applying routing to the SOCKS5 listener. SOCKS5 stays a dumb tunnel (its own
  design D7).
- A smart-listener DNS cache. The system resolver already caches correctly;
  adding a second cache is redundant and risks getting TTLs/negative-answers
  wrong (see D4).
- IP-pinning for plain-HTTP forward requests (only CONNECT pins the RU IP).
- IPv6 geo (matches the PAC's existing IPv4-only stance).
- Per-listener upstream overrides, TLS on the smart listener, client auth.
- Dynamic bind/rebind of the smart listener on config change (restart required,
  same as `listen` and `socks5.listen`).

## Decisions

### D1: Extend `internal/in/http/`, do not create a sibling package

**Choice:** The smart listener lives in a new file `internal/in/http/smart.go`,
in the SAME package as `server.go`. It is wired as a separate `http.Handler`
constructed by a new `NewSmart(...)` entry point, NOT by parameterising the
existing `New(...)`.

**Rationale:**

| Option | Verdict |
|---|---|
| **A. New file in `internal/in/http/`** (chosen) | Shares `copyHeaders`, `buildTransport`, the hijack pattern; HTTP expertise stays in one package; no inverted deps |
| B. New sibling package `internal/in/httpsmart/` | Rejected — would duplicate `forward`/`handleConnect`/`buildTransport`/`copyHeaders` (~150 lines) or invert the dependency (`httpsmart` importing `http`) |
| C. Parameterise existing `New(...)` with a `mode` flag | Rejected — couples dumb and smart paths in one function; a regression in the smart branch can touch the dumb path; the dumb path's pristine status is a stated goal |

The smart handler is a peer of the dumb handler within the package, not a
wrapper around it. Both are constructed independently in `wire.go` and bound to
their own listeners.

**What stays shared vs separate:**

```
internal/in/http/
├── server.go          dumb: New(), forward(), handleConnect(), forwardPlain(),
│                      buildTransport(), copyHeaders(), setPACCacheHeaders()
├── smart.go    NEW    smart: NewSmart(), smartForward(), smartHandleConnect(),
│                      smartDial() (ordered strategies + negcache + RU-IP pin)
├── negcache.go NEW    hostname → expiry map (shared by smart path only)
└── *_test.go          extended with smart_*_test.go
```

`copyHeaders` and `buildTransport` are called by both paths; the rest of
`server.go` is untouched.

### D2: Routing precedence → ordered strategy list (with fallback)

**Choice:** `Decide(...)` returns an ordered `[]Strategy`, not a single
`{Direct | Upstream}` choice. The dialer walks the list in order and uses the
first strategy that produces a live connection.

**Rationale:** The PAC emits a single decision because the browser honours it
statically. The smart listener dials the connection itself and can observe
failure — so it can fall back. The operator confirmed two cases where fallback
matters:

1. **blacklist** (e.g. `z.ai`) — the host is normally reachable directly, but
   occasionally direct fails; falling back to upstream keeps the client working.
2. **RU geo** — the operator explicitly REJECTED fallback here: if a RU host
   cannot be reached directly, the operator will move it to the whitelist rather
   than have the proxy silently route around the geo decision. RU stays
   `[Direct]`, no fallback.

The precedence table:

| Rule | Match by | Strategies (ordered) | Fallback? |
|---|---|---|---|
| blacklist | suffix or CIDR | `[Direct, Upstream]` | yes |
| whitelist | suffix or CIDR | `[Upstream]` | no |
| local/private | plain hostname or private IP | `[Direct]` | no |
| RU geo | any resolved IP in a RU range | `[Direct, pinned to the RU IP]` | no |
| default | catch-all | `[Upstream]` | no |

**Asymmetry with the PAC, documented:** the PAC emits a single decision per
host; the smart listener may emit two (blacklist case) and tries them in order.
Parity holds at the *precedence* level (same rules, same order, same match
criteria); execution differs by design. This is recorded in the spec and in
`docs/architecture.md`.

**Strategy shape:**

```go
type Strategy struct {
    Path   StrategyPath // Direct or Upstream
    DialIP net.IP       // Direct only: nil = dial by hostname; non-nil = dial this IP
}
```

`DialIP` is non-nil ONLY for the RU-geo Direct strategy, where it carries the
specific RU IP the geo rule matched. For blacklist Direct, `DialIP` is nil (dial
by hostname, let the system resolver pick).

### D3: DNS via the system resolver; first-RU-IP-wins

**Choice:** The smart handler resolves each target hostname with
`net.Resolver.LookupIPAddr` (the system resolver, which honours `/etc/hosts`,
DNS, and its own caching). No application-level DNS cache is introduced (see
D4). Among the resolved IPs, the first one that falls in a RU range is selected
for the RU-geo Direct dial; if none are RU, the geo rule does not match.

**Rationale:**

| Question | Answer |
|---|---|
| Why system resolver, not a custom one? | The system resolver already implements caching, negative answers, `/etc/hosts`, and search domains correctly. Reimplementing it is a well-known source of subtle bugs. The project's minimal-deps posture favours the stdlib. |
| Why first-RU-IP-wins, not all-must-be-RU? | Mirrors the PAC's `dnsResolve` semantics (a single IP is returned and checked). `all-must-be-RU` would almost never match for CDN/load-balanced services. `first-RU-IP-wins` is the most useful interpretation of "this host has a RU endpoint." |
| What about privacy? | The proxy now resolves names for PAC-blind clients (it did not before — the upstream did). This is inherent to server-side routing and is documented as a threat-model note for the smart listener only. Browser traffic on the dumb listener is unaffected. |
| What about latency? | One `LookupIPAddr` per CONNECT. The system resolver's cache absorbs the cost for repeated hosts. zcode hits a small set of API endpoints, so the working set is hot. |

**RU-IP selection algorithm:**

```
ips, _ := resolver.LookupIPAddr(ctx, host)
for _, ip := range ips {
    if ip.To4() == nil { continue }   // IPv6 geo is out of scope
    n := ipv4ToUint32(ip.To4())
    if inRuRanges(n, ranges) {
        return Strategy{Path: Direct, DialIP: ip}   // first RU IP wins
    }
}
// no RU IP found → geo rule does not match, fall through to default
```

The binary search over `ranges` reuses the same approach as the PAC's `inRu`
function (the PAC emits it as JavaScript; here it is plain Go over the same
`port.RURanges` data).

### D4: No application-level DNS cache; rely on the system resolver

**Choice:** Deliberately do NOT add a `host → ip` cache. Each request calls
`LookupIPAddr`.

**Rationale:** Correct DNS caching is hard. A correct cache must honour per-RR
TTLs, cache negative answers (with their own TTL semantics), respect
`/etc/hosts` (which can change underneath the process), and handle the
single-flight problem (multiple concurrent lookups for the same name should
share one upstream query). The system resolver already does all of this — and
the operator explicitly flagged that "correct DNS caching is a hard problem and
we rely on the system resolver where it is already implemented."

An application cache would either duplicate this correctly (a lot of subtle
code and tests — out of scope for v1) or duplicate it incorrectly (a latency
optimisation that serves stale answers and breaks routing). Neither is
acceptable.

The negative cache (D6) is a *different* concern — it caches Direct-dial
*connection* failures, not DNS answers, and its TTL is configured by the
operator, not by DNS.

### D5: Negative cache for failed Direct attempts

**Choice:** A small hostname-keyed map records the expiry time of the most
recent Direct-dial failure. Before attempting a Direct dial, the smart handler
checks the cache; if the hostname is still "negative," it skips Direct and
goes straight to the next strategy (typically Upstream). On a Direct failure,
the hostname is marked negative for `smart.negative_cache_ttl`.

**Rationale:** Without a negative cache, every request to a briefly-unreachable
direct host pays the full `dial_attempts * dial_timeout` latency before falling
back. For zcode hitting `z.ai` while z.ai's direct path is down, that is
`2 * 3s = 6s` per request — repeated on every request. The negative cache
collapses this: the first request pays the 6s, subsequent requests within the
TTL go straight to upstream.

**Key choice: hostname, not hostname:port.** Direct-dial failure is almost
always host-level (the host is down, or the route to it is broken), not
per-port. Keying by hostname matches the failure mode and keeps the cache
small. If `z.ai:443` is unreachable, `z.ai:8443` is almost certainly also
unreachable.

**Eviction:** Lazy. The cache does not run a background sweeper; entries are
left in the map and ignored once their expiry passes. A future change can add
active eviction if the map's growth becomes a concern (in practice the working
set is tiny — zcode talks to a handful of hosts).

**Scope:** The negative cache is consulted ONLY on the Direct path of the
blacklist fallback. The RU-geo Direct path does not consult it (no fallback
anyway, per D2). The Upstream path does not consult it (upstream failures are
not cached — they are the operator's upstream, and the operator cannot fix them
by waiting).

**Concurrency:** A `sync.RWMutex`-guarded `map[string]time.Time` is
sufficient. The hot path (read) takes a read lock; writes take a write lock.
`sync.Map` was considered but its read-mostly optimisation is a poor fit here
writes are common (every Direct failure writes).

### D6: Configurable Direct-dial timeout and attempts

**Choice:** `smart.dial_timeout` (default `3s`) bounds each Direct dial attempt.
`smart.dial_attempts` (default `2`) sets how many Direct attempts are made
before the strategy is considered failed. Both are per-request reads from the
live config — hot-reload applies to the next request.

**Rationale:** A Direct attempt that will fail must fail *fast*, or the
fallback is pointless. 3s is short enough that `2 attempts + fallback` stays
under ~7s — acceptable for an interactive client like zcode. But the right
value is network-dependent: a lossy link needs longer (a fast timeout gives up
on a slow-but-working direct path), a fast link wants shorter (so the upstream
fallback kicks in sooner). Hence configurable, with a sane default.

`dial_attempts > 1` covers transient packet loss: a single dropped SYN should
not send the host to the upstream for the next 60s (the negative cache TTL).
Two attempts absorb the common "one retry fixes it" case before recording a
negative.

### D7: PAC is NOT served from the smart listener

**Choice:** The smart listener returns 404 for `/proxy.pac` (or any GET to
that path). It does not share the dumb listener's PAC handler.

**Rationale:** A PAC fetched from the smart port would contain
`PROXY <dumb-listen-addr>`, pointing the client at the dumb listener — which
would be confusing and wrong (the whole point of the smart listener is that
its clients cannot use a PAC). PAC-blind clients do not fetch the PAC anyway;
serving it benefits no one and risks a misconfigured client picking it up and
getting inconsistent routing. Better to 404 and make the absence explicit.

### D8: Opt-in via empty `smart.listen` (mirror `socks5`)

**Choice:** The smart listener starts if and only if `smart.listen` is
non-empty. No separate `smart.enabled` flag. The `dial_timeout`,
`dial_attempts`, and `negative_cache_ttl` fields default when zero/absent.

**Rationale:** Identical to `socks5.listen` (design D3 of the SOCKS5 change).
One knob for "is it on"; the three tuning fields have documented defaults so a
minimal config is just `smart: { listen: 127.0.0.1:8081 }`.

Three config states for `listen`:
- `smart:` block absent → `Listen == ""` → disabled (default; backward compat)
- `smart.listen: ""` explicitly → disabled (operator opt-out)
- `smart.listen: "127.0.0.1:8081"` → enabled

Validation: when `Listen` is non-empty, it MUST parse as `host:port` (reusing
the existing `net.SplitHostPort` check). The tuning fields, when set, MUST be
positive (`dial_timeout > 0`, `dial_attempts > 0`, `negative_cache_ttl > 0`);
zero means "apply the default," so they are always valid in the zero state.

### D9: Parity by property test, not by shared code

**Choice:** Ship `Decide(host, ips, cfg, ranges)` in `internal/app/decide.go`
with its own precedence implementation. Add a property test
(`internal/app/decide_parity_test.go`) that, for a corpus of hosts and a fixed
config+ranges, asserts the server-side decision matches the decision the PAC
would make.

**Rationale:** The alternative — extracting a shared `RoutingModel` from which
both the PAC and `Decide()` are generated — is the architecturally purer choice
but is a much larger refactor of the stable, regex-guarded `pac.go`. The
operator confirmed (explore session) that approach 1 (duplicate logic + parity
tests) is acceptable for v1. The property test is the safety net: any future
change to one side that forgets the other will fail the test.

**Parity check mechanics:** The test constructs a `port.Config` with known
whitelist/blacklist/ranges, renders the PAC via `RenderPAC`, then for each
corpus host evaluates the PAC (via a minimal in-test PAC evaluator or by
extracting the precedence into a shared helper used only by the test) and
compares to `Decide(host, resolvedIPs, cfg, ranges)`. The comparison is on the
*top-priority* strategy: `Decide` returning `[Direct, Upstream]` for a
blacklisted host matches the PAC's `DIRECT`, because the PAC cannot express
fallback — the PAC's single answer is the first strategy `Decide` would try.

## Risks / Trade-offs

- **[PAC and Decide drift over time]** → Mitigation: the property test
  (D9). Any change to precedence that touches only one side fails the parity
  test. The drift risk is no greater than for any duplicated logic with a
  test guard. The long-term fix (shared model, A1) is documented as a
  follow-up.

- **[DNS resolution adds latency to every smart CONNECT]** → Accepted. One
  `LookupIPAddr` per request; the system resolver's cache absorbs repeated
  hosts. zcode's working set is small. If profiling later shows this is hot,
  a follow-up can add a correctly-implemented cache — but the operator
  explicitly preferred relying on the system resolver.

- **[Privacy shift: proxy now resolves names for smart-listener clients]** →
  Accepted and documented. The dumb HTTP listener and SOCKS5 listener are
  unaffected. A PAC-blind client that previously sent its traffic to the
  upstream (which resolved the names) now has the proxy resolve them — the
  names are visible to the local process and, if the resolver is configured to
  use a remote server, to that server. This is inherent in server-side routing
  and is called out in the proposal's threat-model note.

- **[First-RU-IP-wins may pick a slow or broken RU endpoint]** → Accepted.
  Mirrors PAC semantics (one IP, checked once). If a RU endpoint is
  unreachable, the request fails — there is no fallback for RU (D2). The
  operator's remedy is to move the host to the whitelist (forcing upstream),
  which is the documented escape hatch.

- **[Negative cache serves stale "down" entries after the host recovers]** →
  Bounded by `negative_cache_ttl` (default 60s). After the TTL, the next
  request retries Direct and, on success, the entry is effectively ignored
  (lazy eviction). 60s of "go via upstream" for a host that just came back is
  acceptable and self-corrects.

- **[Negative cache grows unbounded over process lifetime]** → Accepted for v1.
  Working set is tiny (zcode's hosts). Lazy eviction ignores expired entries.
  If a long-running process with a huge hostname corpus ever shows this, a
  follow-up can add active eviction — localized change in `negcache.go`.

- **[Operator sets `smart.listen: 0.0.0.0:8081` and exposes an unauth open
  proxy]** → Documented in `config.example.yaml` with the same `127.0.0.1`
  recommendation the HTTP and SOCKS5 sections carry. Not technically prevented
  (matches the other listeners' posture); the threat model explicitly accepts
  localhost-only operation.

## Migration Plan

Three commits, in order, each independently green:

1. **Add `internal/app/decide.go` + parity test.** Pure logic, no I/O. The
   property test asserts parity with the PAC over a fixed corpus. No wiring,
   no listener — `Decide` is callable but unused outside tests. Reviewable as
   a pure addition; existing tests stay green because nothing calls it yet.
2. **Add `internal/in/http/smart.go` + `negcache.go` + `port.Smart` config
   field + loader validation.** New code only; the smart handler is
   constructed by `NewSmart(...)` but NOT yet started from `wire.go`. All
   smart-path unit tests pass; `wire.go` unchanged so the binary behaves
   identically.
3. **Wire the smart listener into `wire.go` + update `config.example.yaml`
   and docs.** Now operators who add a `smart` block get the listener;
   everyone else sees no change.

**Rollback:** revert commit 3 to disable the feature without removing the
code; revert commits 3+2 to remove the code and the config field; revert all
three to return to today's state. Each revert leaves the tree coherent
because each commit is independently green.

**No config migration:** the new `smart` block is optional and defaults to
disabled. Existing `config.yaml` files load unchanged.

## Alternatives Considered

### A1: Extract a shared `RoutingModel` from which both the PAC and `Decide()` are generated

**Decision: deferred (not rejected).** This is the architecturally purer choice
and the long-term home for the precedence logic. Under this design,
`internal/app/routing.go` would hold a `RoutingModel` (rules + ranges) with a
single `Decide(host, ips) → Strategies` method, and `pac.go`'s `RenderPAC`
would consume the same model to emit the JavaScript precedence.

Why deferred for this change:

1. **`pac.go` is stable, regex-guarded, and ES3-constrained.** Its template is
   hand-tuned JavaScript with a test that regex-scans the output for
   forbidden ES3 constructs. Restructuring it to consume a model risks
   breaking the ES3 invariants and the regex guard — a regression surface
   disproportionate to the parity benefit when the parity test (D9) already
   guards correctness.
2. **Scope discipline.** This change's job is "add a smart listener." A
   refactor of `pac.go` is a separate concern with its own risks and review.
   Bundling them couples two reviewable units into one.

The property test (D9) is the bridge: it enforces parity today, and the day a
shared model lands, the test documents the contract the model must uphold.

### A2: Embed a JavaScript runtime (goja) and evaluate the PAC server-side

**Decision: rejected.** Considered for the strongest possible parity (run the
exact same PAC bytes that browsers run). Rejected because:

1. **The "one source of truth" benefit is illusory.** The PAC calls
   `dnsResolve`, `isInNet`, `shExpMatch`, `isPlainHostName` — all of which must
   be provided as host functions by the embedding. Those host functions are
   *new* Go code that can drift from browser behaviour just as easily as
   `Decide()` can. The parity problem moves; it does not disappear.
2. **Adds a dependency for no net benefit.** goja is ~10k LOC of pure-Go JS
   interpreter. The project has two carefully-justified deps (`fsnotify`,
   `yaml.v3`); a JS runtime to avoid ~80 lines of `Decide()` logic is a poor
   trade against the minimal-deps posture.
3. **Latency and GC pressure.** Per-request VM spin-up (or pooling) is heavier
   than a Go function call, in the hot path of every smart-listener request.

`Decide()` + property test achieves the same parity guarantee without these
costs.

### A3: Add the routing to the existing dumb HTTP listener instead of a new port

**Decision: rejected.** This was the explore session's opening idea, abandoned
when the operator clarified the goal. Putting routing on the dumb listener
would:

1. **Change browser behaviour.** Browser clients would have their requests
   re-evaluated server-side after the PAC already directed them. Inconsistent
   results are possible (browser DNS vs proxy DNS may differ for geo-DNS
   hosts).
2. **Conflate two client classes.** Browsers (PAC-aware, want client-side
   decisions) and PAC-blind clients (want server-side decisions) have different
   needs; serving them from one listener forces compromises on both.
3. **Break the "dumb listener is pristine" goal.** Any regression in the
   routing code would affect browser traffic.

The separate port keeps the two populations cleanly separated, each served by
a listener with the right semantics.

### A4: Apply the same routing to the SOCKS5 listener

**Decision: rejected.** SOCKS5's design D7 explicitly excludes routing: the
client (Telegram) decides its own destinations, and applying routing would
risk silently dropping legitimate traffic that matches a blacklist entry. The
smart listener's routing is for PAC-blind HTTP clients; SOCKS5 is a different
client class with different needs. Keeping them separate preserves each
listener's clear contract.

## Open Questions

None blocking. One minor item to settle during implementation:

- **Should the smart listener log a one-line "smart listener bound to X" at
  startup?** Project convention is errors-only, but a single bind confirmation
  (not per-request) may be defensible as operational. The SOCKS5 change left
  the same question open and resolved it by matching the HTTP listener (which
  does not log its bind). Default: stay silent and match the other listeners;
  revisit if operators report confusion about which port is which.
