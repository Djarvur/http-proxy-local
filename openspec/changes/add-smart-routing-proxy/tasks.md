# Tasks — add-smart-routing-proxy

Implementation order follows dependency: scaffolding → pure logic (high test value) → Out adapters → In adapter → wiring → runtime/launchd.

**TDD rule (per `engineering-conventions` spec): every implementation task below is paired with a preceding red-phase test task. Do not check the implementation box without its test passing first.**

## 1. Project scaffolding & tooling

- [x] 1.1 Create `go.mod` (module `github.com/Djarvur/http-proxy-local`, latest stable Go), `cmd/http-proxy-local/{main.go,wire.go}` stubs, and the `internal/{port,app,in/http,out/{ripe,fscache,yamlconfig,fsnotify}}` directory tree per design D1. No `.go` files at the module root.
- [x] 1.2 Write a failing test asserting `golangci-lint run` exits 0 on the empty project (drives the existence of a strict config).
- [x] 1.3 Create `.golangci.yml` enabling `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gocyclo`, `gosec`, `revive`, `gocritic`, findings-as-errors. Verify task 1.2 passes.
- [x] 1.4 Add `Makefile` targets: `test`, `lint`, `run`, `build`. Verify `make lint` and `make test` pass.
- [x] 1.5 Create `mise.toml` at the repo root pinning Go (latest stable) and `golangci-lint` versions, with tasks mirroring the Makefile: `build`, `test`, `lint`, `run`. Verify `mise install` then `mise run test`/`mise run lint` pass from a clean clone.

## 2. Port & types (the contract)

- [x] 2.1 Write failing tests for `port/types.go` structs (`Config`, `Upstream`, `Geo`, `Routing`, `RURanges`, `PACInput`, sentinel errors).
- [x] 2.2 Implement `internal/port/types.go` to pass 2.1.
- [x] 2.3 Write failing tests for `port/port.go` interfaces (`App`, `Upstream` Out port, `GeoSource` Out port, `Cache` Out port, `Clock` Out port).
- [x] 2.4 Implement `internal/port/port.go` to pass 2.3.

## 3. Pure logic: RIPE parse + range merge (app/ranges.go)

- [x] 3.1 Write failing tests for `app.ParseRIPELine`: valid RU ipv4 line → correct `(low, high)`; non-RU country → skip; non-ipv4 type → skip; malformed → error.
- [x] 3.2 Implement `ParseRIPELine` to pass 3.1.
- [x] 3.3 Write failing tests for `app.MergeRanges`: disjoint input preserved; adjacent merged (`[10,19]+[20,29]→[10,29]`); overlapping merged (`[10,30]+[20,40]→[10,40]`); unsorted input gets sorted; empty input → empty output.
- [x] 3.4 Implement `MergeRanges` to pass 3.3.
- [x] 3.5 Write failing tests for `app.ParseAndMerge(rawText)`: end-to-end from delegation text to merged ranges; malformed input → error WITHOUT partial output.
- [x] 3.6 Implement `ParseAndMerge` to pass 3.5.

## 4. Pure logic: PAC rendering (app/pac.go)

- [x] 4.1 Write failing test: rendered PAC contains `PROXY <listen>` with the configured listen address.
- [x] 4.2 Write failing test: rendered PAC is valid ES3-subset — no arrow functions, no template literals, no `const`/`let` (regex scan over output).
- [x] 4.3 Write failing test: rendered `RU_RANGES` causes `inRu(ipToInt("1.2.3.5"))` to be true when range for `1.2.3.0/24` is present, false otherwise.
- [x] 4.4 Write failing test: empty `RURanges` renders an empty `RU_RANGES` array (degradation case).
- [x] 4.5 Implement `app.RenderPAC(input PACInput) []byte` and the template constant to pass 4.1–4.4.
- [x] 4.6 Write a JS-executable harness test (using `node` via `exec.Command`, or an embedded JS engine) that loads the rendered PAC and asserts on `FindProxyForURL` for all precedence scenarios from the `pac-generation` spec: blacklist-over-whitelist, whitelist-over-local, whitelist-over-RU, local-not-listed, RU-not-listed, non-RU-not-listed, suffix match, CIDR match, plain hostname, loopback, IPv6 fallthrough.
- [x] 4.7 Fix any PAC template defects surfaced by 4.6 until all precedence scenarios pass.

## 5. Out adapter: yamlconfig (loader + validator)

- [x] 5.1 Write failing tests for `yamlconfig.Load(path)`: valid YAML → typed Config with defaults applied (listen=`127.0.0.1:8080`, refresh_interval=`24h`, retry_interval=`5m`, source_url=default); missing fields use defaults.
- [x] 5.2 Implement `Load` to pass 5.1.
- [x] 5.3 Write failing tests for `yamlconfig.Validate`: `listen` parses as `host:port`; `upstream.url` is http/https; `refresh_interval` and `retry_interval` are positive durations; invalid cases return wrapped errors.
- [x] 5.4 Implement `Validate` to pass 5.3.

## 6. Out adapter: fscache (atomic read/write)

- [x] 6.1 Write failing tests for `fscache.Write(path, ranges, ts)`: file appears at path after write; content round-trips through `Read`; write is atomic (temp-then-rename — assert no `.tmp` file left behind on success, and a simulated crash mid-write leaves the previous file intact).
- [x] 6.2 Implement `Write` (temp-then-rename) and `Read` to pass 6.1.
- [x] 6.3 Write failing test for `fscache.Freshness(path, interval, now)`: returns fresh when `now - ts < interval`, stale otherwise, missing-file → not-fresh.
- [x] 6.4 Implement `Freshness` to pass 6.3.

## 7. Out adapter: ripe (HTTPS fetch)

- [x] 7.1 Write failing tests for `ripe.Fetch(ctx, url)`: stub an `httptest.Server` returning canned RIPE text → returns that text; non-2xx → error; timeout (use a server that sleeps + short ctx) → error.
- [x] 7.2 Implement `Fetch` (HTTPS client with configurable timeout) to pass 7.1.

## 8. Out adapter: fsnotify watcher (debounced)

- [x] 8.1 Write failing tests for the debounced watcher: write config file twice within 100ms → exactly one reload signal on the output channel; write-temp-then-rename → exactly one signal.
- [x] 8.2 Implement the debounced watcher in `internal/out/fsnotify/watcher.go` to pass 8.1.

## 9. In adapter: HTTP server + forwarder

- [x] 9.1 Write failing tests for the `/proxy.pac` handler: returns 200, `Content-Type: application/x-ns-proxy-autoconfig`, body equals current `pacPtr` snapshot; after a snapshot swap, returns the new bytes.
- [x] 9.2 Implement the PAC handler (reads `pacPtr.Load()`) to pass 9.1.
- [x] 9.3 Write failing tests for plain-HTTP forwarding: client sends `GET http://example/...` → request reaches a stub upstream with method/URL/headers/body preserved; upstream response relayed unchanged.
- [x] 9.4 Implement the plain-HTTP forwarder (reads `configPtr.Load()` for upstream + basic auth) to pass 9.3.
- [x] 9.5 Write failing tests for CONNECT tunneling: `CONNECT host:443` → bidirectional byte copy between client and stub upstream; both sides close cleanly.
- [x] 9.6 Implement the CONNECT handler to pass 9.5.
- [x] 9.7 Write failing tests for basic-auth injection: credentials configured → `Authorization: Basic <b64>` header present on forwarded request; credentials empty → no header added, client `Proxy-Authorization` forwarded unchanged.
- [x] 9.8 Implement basic-auth injection to pass 9.7 (this is part of 9.4 but verify with a dedicated test).
- [x] 9.9 Write failing test: hot-reloaded upstream URL is applied to the next forwarded request without restart (configPtr swap mid-test).
- [x] 9.10 Verify 9.9 passes against the live config read pattern.

## 10. App layer: orchestration (reload + PAC regen coordination)

- [x] 10.1 Write failing tests for the PAC regenerator goroutine: signal on `pacRegenCh` → renders from current configPtr + ranges, atomically swaps pacPtr; multiple rapid signals coalesce into one render.
- [x] 10.2 Implement the regenerator goroutine to pass 10.1.
- [x] 10.3 Write failing tests for config-reload orchestration: valid config swap → configPtr updated + pacRegen signaled (if PAC-affecting fields changed) + NOT signaled (if only upstream changed); invalid config → configPtr unchanged + error logged + no signal.
- [x] 10.4 Implement the reload orchestrator to pass 10.3.
- [x] 10.5 Write failing test: `listen` change → error logged mentioning restart-required; listener NOT rebound.
- [x] 10.6 Implement the listen-change guard to pass 10.5.

## 11. Geo fetcher state machine

- [x] 11.1 Write failing tests for startup behavior: fresh cache → no fetch attempted, ranges loaded, PAC regen signaled; stale cache → blocking fetch attempted, on success cache rewritten; no cache → blocking fetch attempted, on success cache written.
- [x] 11.2 Implement startup logic to pass 11.1 (using fakes for GeoSource/Cache/Clock).
- [x] 11.3 Write failing tests for fetch-failure degradation: stale cache + fail → serve stale, log error, no crash; no cache + fail → empty ranges, log error, process continues, PAC regen signaled with empty ranges.
- [x] 11.4 Implement failure handling to pass 11.3.
- [x] 11.5 Write failing tests for the ticker loop, covering the dual-interval schedule: (a) success → next tick at `refresh_interval`; (b) failure → next tick at `retry_interval` (default 5m); (c) consecutive failures keep retrying at `retry_interval`; (d) a success after failures reverts to `refresh_interval`; (e) successful refresh rewrites cache + signal PAC regen.
- [x] 11.6 Implement the ticker loop (single goroutine, `nextInterval` chosen from last-outcome) to pass 11.5.
- [x] 11.7 Write failing test for hot-reload of intervals: changing `geo.refresh_interval` or `geo.retry_interval` mid-flight does not shorten the in-progress wait, but the next tick uses the new value based on its outcome.
- [x] 11.8 Implement the hot-reload-aware interval selection to pass 11.7.

## 12. Composition root & runtime

- [x] 12.1 Write failing integration test (`cmd/http-proxy-local/wire_test.go` + `internal/app/runtime_test.go`): given a temp config dir + stub RIPE server, the wired Runtime serves a PAC at `/proxy.pac`, forwards a request to a stub upstream, and reloads on config edit.
- [x] 12.2 Implement `cmd/http-proxy-local/wire.go` (composition root, `package main`) and `internal/app/runtime.go` to pass 12.1.
- [x] 12.3 Write `cmd/http-proxy-local/main.go`: parse flags (config path override), signal handling (SIGINT/SIGTERM → graceful shutdown), call `wire.Run`.

## 13. CI — GitHub Actions

- [x] 13.1 Create `.github/workflows/pr.yml` triggered on PR + push to main: install Go (via the `mise.toml` pinning from 1.5) and `golangci-lint`; run `go test ./... -race -coverprofile=cover.out`; enforce a coverage floor (e.g. fail if total coverage drops below a threshold; record the threshold in the workflow as a variable so it's tunable); run `golangci-lint run` (strict config from 1.3, must be 0 findings). Fail the check on any red step. Upload `cover.out` as a build artifact.
- [x] 13.2 Create `.github/workflows/periodic.yml` triggered on schedule (e.g. `cron: "0 6 * * 1"` weekly) and manual dispatch: (a) `go test ./...` — catches dependency regressions; (b) dependency update check — run `go list -m -u all` (or `govulncheck`) and surface outdated/vulnerable modules; (c) `golangci-lint run` against the latest linter release to catch newly-flagged patterns. On any failure, open an issue (or fail loudly) so it's not silently ignored.

## 14. Hardening & acceptance

- [x] 14.1 Run `make lint` and resolve any findings at the source (no suppressions). Confirm zero findings.
- [x] 14.2 Run `make test` end-to-end; confirm all green.
- [x] 14.3 Manual smoke test: start binary against the real RIPE URL, point macOS PAC at `http://127.0.0.1:8080/proxy.pac`, verify RU site routes DIRECT and non-RU routes through upstream (verify via upstream logs or a tcpdump).
- [x] 14.4 Manual hot-reload smoke test: edit config whitelist while running, verify next `/proxy.pac` GET returns updated bytes within the debounce window.
- [x] 14.5 Verify each capability spec scenario has a corresponding passing test or manual demonstration; record any scenario covered only manually in the change archive notes.
