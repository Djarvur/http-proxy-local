# Tasks — add-smart-http-listener

Implementation order follows the design's three-commit migration plan:
add `Decide()` + parity test → add `smart.go`/`negcache.go` + `port.Smart`
config field (unwired) → wire into `wire.go`. Each intermediate commit SHALL
be independently green.

TDD convention (engineering-conventions spec): every task that adds production
behavior has a preceding test-writing task. Phase 1 (`Decide`) is pure logic
and fully TDD'd. Phases 2 and 3 follow the same red/green/refactor cycle.

## 1. Add `Decide()` pure logic + PAC parity test

- [x] 1.1 Write `internal/app/decide_test.go` with table-driven tests over `Decide(host, ips, cfg, ranges)` covering: blacklist suffix → `[Direct, Upstream]`; blacklist CIDR → `[Direct, Upstream]`; whitelist suffix → `[Upstream]`; whitelist CIDR → `[Upstream]`; plain hostname → `[Direct]`; private IP (10.x/172.16.x/192.168.x/127.x/169.254.x) → `[Direct]`; RU IPv4 in range → `[Direct]` with `DialIP` set to the matched RU IP; RU IPv4 not in range → falls through to default; default catch-all → `[Upstream]`; blacklist+whitelist both match → blacklist wins (`[Direct, Upstream]`); multiple resolved IPs with one RU → first RU IP wins; IPv6-only resolved IPs → geo does not match, falls through
- [x] 1.2 Implement `internal/app/decide.go`: `Strategy` struct (`Path: Direct|Upstream`, `DialIP net.IP`), `Decide(host string, ips []net.IP, cfg port.Config, ranges port.RURanges) []Strategy`. Pure function, no I/O. Helpers: `matchSuffix(host, patterns)`, `matchCIDR(ips, patterns)`, `isPlain(host)`, `isPrivate(ips)`, `firstRU(ips, ranges) (net.IP, bool)` — binary search over `ranges` mirroring the PAC's `inRu` approach
- [x] 1.3 Run `go test ./internal/app/ -run Decide`; green
- [x] 1.4 Run `golangci-lint run`; zero findings
- [x] 1.5 Write `internal/app/decide_parity_test.go`: a property test that, for a fixed corpus of hosts (suffix matches, CIDR matches, plain hostnames, RU IPs, non-RU IPs, IPv6) and a fixed `port.Config` + `port.RURanges`, asserts that `Decide(...)`'s top-priority strategy matches the decision the rendered PAC would make. Evaluate the PAC via a minimal in-test helper that captures the precedence (or render the PAC via `RenderPAC` and check the emitted matchers' first-match against each corpus host). Blacklist `Decide` result `[Direct, Upstream]` SHALL match PAC `DIRECT` (the PAC's single answer is the first strategy `Decide` would try)
- [x] 1.6 Run `go test ./internal/app/ -run Parity`; green
- [x] 1.7 Run `golangci-lint run`; zero findings
- [x] 1.8 Commit as a pure addition (single commit; no wiring; existing tests stay green because nothing calls `Decide` outside tests yet)

## 2. Add `Smart` struct to `port.Config`

- [x] 2.1 Write/extend `internal/port/types_test.go` test asserting `Config` round-trips a `Smart{Listen: "127.0.0.1:8081", DialTimeout: 3s, DialAttempts: 2, NegativeCacheTTL: 60s}` field through YAML marshal/unmarshal and that an absent block leaves all fields zero-valued
- [x] 2.2 Add `Smart struct { Listen string; DialTimeout time.Duration; DialAttempts int; NegativeCacheTTL time.Duration }` to `port.Config` and a `Smart Smart \`yaml:"smart"\`` field; no `applyDefaults` change for `Listen` (D8: empty means disabled); `applyDefaults` SHALL fill the three tuning fields when zero
- [x] 2.3 Run `go test ./internal/port/...`; green
- [x] 2.4 Run `golangci-lint run`; zero findings

## 3. Validate `smart.*` fields in config loader

- [x] 3.1 Write/extend `internal/out/yamlconfig/loader_test.go` tests: (a) non-empty `smart.listen` that does not parse as `host:port` fails validation wrapping `port.ErrValidation` and naming the field; (b) empty/absent `smart.listen` passes; (c) non-positive `smart.dial_timeout` fails naming the field; (d) non-positive `smart.dial_attempts` fails naming the field; (e) non-positive `smart.negative_cache_ttl` fails naming the field; (f) omitted tuning fields with non-empty `smart.listen` pass and the loader applies the documented defaults
- [x] 3.2 Extend `yamlconfig/loader.go` `Validate`: when `smart.listen` is non-empty, `net.SplitHostPort` MUST succeed (mirror the existing `listen`/`socks5.listen` checks); when `smart.dial_timeout != 0`, it MUST be positive; same for `negative_cache_ttl`; `smart.dial_attempts` MUST be positive when non-zero. Add the three defaults to `applyDefaults` (`dial_timeout: 3s`, `dial_attempts: 2`, `negative_cache_ttl: 60s`)
- [x] 3.3 Run `go test ./internal/out/yamlconfig/...`; green
- [x] 3.4 Run `golangci-lint run`; zero findings

## 4. Negative cache for failed Direct attempts

- [x] 4.1 Write `internal/in/http/negcache_test.go` with table-driven tests: (a) `IsNegative(host)` returns false for an unseen host; (b) `MarkNegative(host, ttl)` then `IsNegative(host)` returns true; (c) after the TTL elapses (use a fake clock or a `time.Now` override), `IsNegative` returns false; (d) marking a host negative again extends the expiry; (e) concurrent `MarkNegative` and `IsNegative` do not race (run under `-race`)
- [x] 4.2 Implement `internal/in/http/negcache.go`: a `negativeCache` struct with a `sync.RWMutex`-guarded `map[string]time.Time` (hostname → expiry). Methods: `IsNegative(host string) bool` (read-locks, checks `time.Now().Before(expiry)`), `MarkNegative(host string, ttl time.Duration)` (write-locks, sets expiry to `time.Now().Add(ttl)`). Lazy eviction — expired entries remain in the map but are treated as not-negative
- [x] 4.3 Run `go test ./internal/in/http/ -run NegativeCache`; green
- [x] 4.4 Run `golangci-lint run`; zero findings

## 5. Smart handler — strategy-driven dialing

- [x] 5.1 Write `internal/in/http/smart_test.go` with unit tests over the dial-selection logic (extracted as a pure helper where possible): (a) given a `[Direct, Upstream]` list and a Direct that succeeds, returns the Direct conn; (b) given the same list and a Direct that fails (after `dial_attempts`), falls back to Upstream and returns the Upstream conn; (c) given a `[Direct]` list (RU geo) and a Direct that fails, returns the error (no fallback); (d) when `negativeCache.IsNegative(host)` is true, the Direct strategy is skipped; (e) on Direct failure, `MarkNegative` is called with the configured TTL. Use fake dialers (functions returning canned `net.Conn`/error) — no real network
- [x] 5.2 Implement `internal/in/http/smart.go`: `NewSmart(configPtr *atomic.Pointer[port.Config], rangesPtr *atomic.Pointer[port.RURanges], resolver *net.Resolver, negCache *negativeCache) http.Handler`. Handler logic: for CONNECT → `smartHandleConnect`; for plain HTTP → `smartForward`. Shared `smartDial(ctx, host, port, cfg, ranges) (net.Conn, error)`: resolve (only if needed by precedence), call `Decide`, walk strategies (Direct: check negCache → dial by hostname or `DialIP` with `dial_timeout`, up to `dial_attempts` → on failure `MarkNegative` and continue; Upstream: `cascade.Dial(cfg.Upstream, host:port)`). Reuses `buildTransport`, `copyHeaders` from `server.go`
- [x] 5.3 Run `go test ./internal/in/http/ -run Smart`; green
- [x] 5.4 Run `golangci-lint run`; zero findings

## 6. Smart handler — CONNECT end-to-end

- [x] 6.1 Extend `internal/in/http/smart_test.go` with end-to-end CONNECT tests using a real `net.Listener` + in-process fake targets and a fake upstream (mirror `internal/in/http/chain_test.go`'s pattern): (a) CONNECT to a blacklisted host with a working direct target → `200`, tunnel bytes both ways; (b) CONNECT to a blacklisted host with a dead direct target and a working upstream → `200` via fallback, tunnel bytes; (c) CONNECT to a RU host (fake-resolved to a RU IP) → dials the RU IP, `200`, tunnel bytes; (d) CONNECT where all strategies fail → connection closed, failure logged; (e) GET `/proxy.pac` → 404
- [x] 6.2 Implement `smartHandleConnect` in `internal/in/http/smart.go`: hijack, evaluate precedence, `smartDial`, on success write `HTTP/1.1 200 Connection Established\r\n\r\n` and `cascade.Tunnel`; on failure close and log. For the RU-geo Direct strategy, dial `DialIP:port` directly; for other Direct strategies, dial `host:port`
- [x] 6.3 Run `go test ./internal/in/http/ -run SmartConnect`; green
- [x] 6.4 Run `golangci-lint run`; zero findings

## 7. Smart handler — plain HTTP forward

- [x] 7.1 Extend `internal/in/http/smart_test.go` with plain-HTTP tests: (a) absolute-URI request for a blacklisted host, direct target works → forwarded directly, response relayed verbatim; (b) absolute-URI request for a default (Upstream) host → forwarded via the fake upstream, response relayed; (c) RU host → dialed by hostname (NOT by RU IP — assert the dialed address is the hostname); (d) non-absolute-URI request → 400 Bad Request
- [x] 7.2 Implement `smartForward` in `internal/in/http/smart.go`: reject non-absolute-URI with 400; evaluate precedence; select first successful strategy; for Direct build a transport with no `Proxy` (dials by hostname); for Upstream build a transport via the shared `buildTransport`; relay response headers and body via `copyHeaders` + `io.Copy`. No IP-pinning (the transport's own dial uses the hostname)
- [x] 7.3 Run `go test ./internal/in/http/ -run SmartForward`; green
- [x] 7.4 Run `golangci-lint run`; zero findings

## 8. Reload orchestrator — `smart.listen` restart-required downstream effect

- [x] 8.1 Write/extend `internal/app/orchestrator_test.go` test asserting: a reload that changes `smart.listen` while running logs a restart-required error and does NOT rebind. Cover all three transitions: address changed, `smart` block added (was absent → now present), `smart` block removed (was present → now absent). Also assert that a change to `smart.dial_timeout`/`dial_attempts`/`negative_cache_ttl` does NOT emit a restart-required error (applies per-request)
- [x] 8.2 Extend `ReloadOrchestrator.Reload` in `internal/app/runtime.go` to detect `smart.listen` changes (and block add/remove) alongside the existing `listen` and `socks5.listen` checks, emitting a restart-required log of the same shape
- [x] 8.3 Run `go test ./internal/app/ -run Orchestrator`; green
- [x] 8.4 Run `golangci-lint run`; zero findings

## 9. Wire smart listener into composition root

- [x] 9.1 Extend `cmd/http-proxy-local/wire_test.go` with tests asserting: (a) when `initial.Smart.Listen != ""`, the dumb HTTP, SOCKS5 (if configured), and smart HTTP listeners all bind; a smart-listener client can CONNECT through the chain; (b) cancelling the context shuts down all configured `Serve` goroutines within 10s; (c) when `initial.Smart.Listen == ""`, no smart port binds and behavior is byte-for-byte identical to a build without this feature; (d) the dumb listener still serves `/proxy.pac` and still forwards dumb-path traffic unaffected
- [x] 9.2 Extend `wire.go` `Run`: after the SOCKS5 listener block, when `initial.Smart.Listen != ""`, bind a smart HTTP listener, construct `httpserver.NewSmart(configPtr, rangesPtr, net.DefaultResolver, negCache)`, and launch its `Serve` in a goroutine; coordinate shutdown with the other `Serve` goroutines and `ctx` via select (mirrors the existing two-listener pattern, extended to three) — any listener failing unexpectedly tears down the others
- [x] 9.3 Run `go test ./cmd/http-proxy-local/...` (includes existing `e2e_test.go`); green
- [x] 9.4 Run `golangci-lint run`; zero findings

## 10. Docs and config example

- [x] 10.1 Add a documented `smart:` block to `config.example.yaml` with the recommended `127.0.0.1:8081` value, the "empty = disabled" rule, the "no activating default port" rule, the three tuning fields with their defaults and rationale, and the localhost-only posture note
- [x] 10.2 Add a "Using the smart listener for PAC-blind clients (zcode)" section to `README.md` covering: where to point the client (`HTTP_PROXY`/`HTTPS_PROXY` at `http://127.0.0.1:8081`), what it does (applies PAC-equivalent routing server-side so DIRECT/Upstream is chosen for the client), what it does NOT do (no PAC served, no routing on the SOCKS5 listener), the blacklist-fallback behavior (direct-first with upstream fallback for blacklisted hosts like `z.ai`), and the negative-cache behavior (failed direct attempts are remembered for `negative_cache_ttl`)
- [x] 10.3 Add a paragraph to `docs/architecture.md` noting the third inbound adapter, the deliberate server-side-vs-PAC parity (precedence parity, execution differs — the smart listener may fall back), the fallback-only-for-blacklist asymmetry, the system-resolver DNS stance (no application cache), and the negative cache. Reference design D1–D9 and the three-commit migration
- [x] 10.4 Update the README routing-precedence table to note that the smart listener applies the same precedence server-side, and that the dumb listener and SOCKS5 listener are unaffected

## 11. Final verification gate

- [x] 11.1 Run `golangci-lint run` from repo root against the existing `.golangci.yml`; zero findings required
- [x] 11.2 Run `go test -race ./...`; all pass, race detector clean
- [x] 11.3 Verify the three-commit structure: commit 1 (`decide.go` + parity test) compiles and passes tests alone; commits 1+2 (smart handler + config field, unwired) compile and pass tests alone; commits 1+2+3 (full wire-in) is the final state. Each intermediate commit is independently green
- [x] 11.4 Optional manual smoke test: configure `smart.listen: 127.0.0.1:8081`, point `zcode` at it, confirm (a) requests to a blacklisted direct host go direct and fall back to upstream when direct is down; (b) requests to a RU host go direct; (c) requests to everything else go via upstream. Record the outcome in the change's archive notes
