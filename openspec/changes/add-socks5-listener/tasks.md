# Tasks — add-socks5-listener

Implementation order follows the design's three-commit migration plan:
extract `cascade/` → add `socks5/` package + config field (unwired) → wire
into `wire.go`. Each intermediate commit SHALL be independently green.

TDD convention (engineering-conventions spec): every task that adds production
behavior has a preceding test-writing task. Phase 1 is a pure refactor with no
new behavior — its discipline is "existing HTTP tests stay green," not a red
phase, because there is no new behavior to express in a failing test.

## 1. Extract upstream cascade into `internal/in/cascade/` (pure refactor)

- [x] 1.1 Move `dialUpstream`, `dialProxy`, `tlsDial`, `tunnel`, and `bufferedConn` out of `internal/in/http/` (server.go + helpers.go) into a new `internal/in/cascade/` package
- [x] 1.2 Expose the entry point as `cascade.Dial(u port.Upstream, target string) (net.Conn, error)` and the tunnel helper as `cascade.Tunnel(a, b net.Conn)`; keep `bufferedConn` unexported inside the package
- [x] 1.3 Update `internal/in/http/server.go` (`handleConnect`) to call `cascade.Dial` + `cascade.Tunnel` instead of the now-moved local symbols
- [x] 1.4 Run `go test ./internal/in/http/...` and `go test -tags realproxy ./internal/in/http/...` (if `$ZCODE_HTTP_PROXY` set) — all existing tests MUST stay green; this is the regression guard for the extraction
- [x] 1.5 Run `golangci-lint run`; zero findings required
- [x] 1.6 Commit as a pure refactor (single commit; no behavior change; reviewable in isolation)

## 2. Add `SOCKS5` struct to `port.Config`

- [x] 2.1 Write `internal/port/types_test.go` test asserting `Config` round-trips a `SOCKS5{Listen: "127.0.0.1:1080"}` field through YAML marshal/unmarshal and that an absent block leaves `Listen == ""`
- [x] 2.2 Add `SOCKS5 struct { Listen string \`yaml:"listen"\` }` to `port.Config` and a `SOCKS5 SOCKS5 \`yaml:"socks5"\`` field; no `applyDefaults` change (D3: empty means disabled, no activating default port)
- [x] 2.3 Run `go test ./internal/port/...`; green
- [x] 2.4 Run `golangci-lint run`; zero findings

## 3. Validate `socks5.listen` in config loader

- [x] 3.1 Write `internal/out/yamlconfig/loader_test.go` test asserting a non-empty `socks5.listen` that does not parse as `host:port` fails validation with an error wrapping `port.ErrValidation` and naming the `socks5.listen` field
- [x] 3.2 Write loader_test asserting empty/absent `socks5.listen` passes validation (the disabled state is valid)
- [x] 3.3 Implement validation in `yamlconfig/loader.go` `Validate`: when `socks5.listen` is non-empty, `net.SplitHostPort` MUST succeed; mirror the existing `listen` field check
- [x] 3.4 Run `go test ./internal/out/yamlconfig/...`; green
- [x] 3.5 Run `golangci-lint run`; zero findings

## 4. SOCKS5 pure framing — method negotiation

- [x] 4.1 Write `internal/in/socks5/negotiate_test.go` with table-driven tests over `bytes.Buffer` pairs: valid greeting offering `0x00` → writes method byte `0x00`; greeting without `0x00` → writes `0xFF`; first byte not `0x05` → returns error without writing; truncated greeting (fewer bytes than declared NMETHODS, or <2 total) → returns error
- [x] 4.2 Implement `negotiateMethod(r io.Reader, w io.Writer) error` per design D5/D6 — pure function, no `net` dependency
- [x] 4.3 Run `go test ./internal/in/socks5/ -run Negotiate`; green
- [x] 4.4 Run `golangci-lint run`; zero findings

## 5. SOCKS5 pure framing — request parsing

- [x] 5.1 Write `internal/in/socks5/request_test.go` with table-driven tests: CONNECT+IPv4 → `(0x01, "1.2.3.4:443", nil)`; CONNECT+domain → `(0x01, "example.com:443", nil)`; CONNECT+IPv6 → `(0x01, "[2001:db8::1]:443", nil)`; BIND request → returns cmd `0x02` (caller decides refusal); truncated request → error; malformed ATYP → error
- [x] 5.2 Implement `readRequest(r io.Reader) (cmd byte, host string, port uint16, err error)` per D5/D6 — IPv6 hosts formatted as `[addr]:port` per RFC 3986; no DNS resolution (the upstream CONNECT resolves)
- [x] 5.3 Run `go test ./internal/in/socks5/ -run ReadRequest`; green
- [x] 5.4 Run `golangci-lint run`; zero findings

## 6. SOCKS5 pure framing — reply writing

- [x] 6.1 Write `internal/in/socks5/reply_test.go` asserting `writeReply(w, 0x00)` produces exactly the 10-byte success reply (VER=5, REP=0, RSV=0, ATYP=1, BND.ADDR=0.0.0.0, BND.PORT=0); `writeReply(w, 0x07)` produces command-not-supported; `writeReply(w, 0x01)` produces general failure
- [x] 6.2 Implement `writeReply(w io.Writer, code byte) error` — always emits BND.ADDR `0.0.0.0:0` (D5)
- [x] 6.3 Run `go test ./internal/in/socks5/ -run WriteReply`; green
- [x] 6.4 Run `golangci-lint run`; zero findings

## 7. SOCKS5 connection shell — `Serve`

- [x] 7.1 Write `internal/in/socks5/serve_test.go` using a real `net.Listener` + an in-process fake HTTP upstream (mirroring `internal/in/http/chain_test.go`'s pattern). End-to-end scenarios: (a) CONNECT to a fake host tunnels bytes both ways through the fake upstream; (b) BIND request → shell writes `0x07` and closes; (c) UDP ASSOCIATE → `0x07` and closes; (d) cascade failure (fake upstream rejects CONNECT) → `0x01` reply; (e) client holds handshake open past deadline → connection closed; (f) a request matching the live config's blacklist/whitelist/RU-geo is tunneled anyway (no routing applied)
- [x] 7.2 Implement `Serve(ctx context.Context, ln net.Listener, cfgPtr *atomic.Pointer[port.Config])` per D6: Accept loop, per-connection goroutine, set 10s handshake read deadline, call `negotiateMethod` → `readRequest`, dispatch on cmd (CONNECT → proceed; BIND/UDP/unknown → `writeReply(0x07)` + close), call `cascade.Dial(cfg.Upstream, target)` → on success `writeReply(0x00)` + clear deadline + `cascade.Tunnel`; on failure `writeReply(0x01)` + close. Read `cfgPtr.Load().Upstream` per-connection (D4). Exit on `<-ctx.Done()` and close the listener.
- [x] 7.3 Run `go test ./internal/in/socks5/ -run Serve`; green
- [x] 7.4 Run `golangci-lint run`; zero findings

## 8. Reload orchestrator — `socks5.listen` restart-required downstream effect

- [x] 8.1 Write/extend `internal/app/orchestrator_test.go` test asserting: a reload that changes `socks5.listen` while running logs a restart-required error and does NOT rebind. Cover all three transitions: address changed, `socks5` block added (was absent → now present), `socks5` block removed (was present → now absent)
- [x] 8.2 Extend `ReloadOrchestrator.Reload` to detect `socks5.listen` changes alongside the existing `listen` check and emit a restart-required log of the same shape (spec scenario: "socks5.listen change logs restart-required error")
- [x] 8.3 Run `go test ./internal/app/ -run Orchestrator`; green
- [x] 8.4 Run `golangci-lint run`; zero findings

## 9. Wire SOCKS5 listener into composition root

- [x] 9.1 Extend `cmd/http-proxy-local/wire_test.go` with tests asserting: (a) when `initial.SOCKS5.Listen != ""`, both HTTP and SOCKS5 listeners bind; a SOCKS5 client can CONNECT through the chain; (b) cancelling the context shuts down both `Serve` goroutines within 10s; (c) when `initial.SOCKS5.Listen == ""`, no SOCKS5 port binds and behavior is byte-for-byte identical to a build without this feature
- [x] 9.2 Extend `wire.go` `Run`: after the HTTP listener block, when `initial.SOCKS5.Listen != ""`, bind a SOCKS5 listener and launch `socks5.Serve` in a goroutine; coordinate shutdown with the HTTP `Serve` goroutine and `ctx` via select (D2) — either listener failing unexpectedly tears down the other
- [x] 9.3 Run `go test ./cmd/http-proxy-local/...` (includes existing `e2e_test.go`); green
- [x] 9.4 Run `golangci-lint run`; zero findings

## 10. Docs and config example

- [x] 10.1 Add a documented `socks5:` block to `config.example.yaml` with the recommended `127.0.0.1:1080` value, the "empty = disabled" rule, the "no activating default port" rule, and the localhost-only posture note
- [x] 10.2 Add a "Using the SOCKS5 listener with Telegram" section to `README.md` covering: where to point the Telegram client (SOCKS5, 127.0.0.1:1080, no auth), what works (messages, media, channels), what does not (P2P voice/video over UDP — TCP fallback expected), and the rationale (why not MTProto)
- [x] 10.3 Add a paragraph to `docs/architecture.md` noting the second inbound adapter and the shared `cascade/` package; reference design D1 and the three-commit migration
- [x] 10.4 Update any capability index or README routing-precedence table to note that SOCKS5 traffic is exempt from PAC/geo routing (D7)

## 11. Final verification gate

- [x] 11.1 Run `golangci-lint run` from repo root against the existing `.golangci.yml`; zero findings required (the strict config is already in place per engineering-conventions spec)
- [x] 11.2 Run `go test -race ./...`; all pass, race detector clean
- [x] 11.3 Verify the three-commit structure: commit 1 (extract `cascade/`) compiles and passes tests alone; commits 1+2 (extract + SOCKS5 package + config field) compile and pass tests alone; commits 1+2+3 (full wire-in) is the final state. Each intermediate commit is independently green.
- [ ] 11.4 Optional manual smoke test: with `$ZCODE_HTTP_PROXY` set, run `go test -tags realproxy ./...` to exercise the real HTTPS-CONNECT cascade end-to-end
- [ ] 11.5 Empirical Telegram verification (deferred from proposal): point a real Telegram client at `socks5://127.0.0.1:1080`, confirm messages/media/channels work, and document whether voice/video calls succeed via TCP fallback. This is the real-world check the design's Risk section flagged — it is not a code task but an acceptance task; record the outcome in the change's archive notes.
