# Tasks

## 1. Extract the shared dial chain (design D1)

- [x] 1.1 Write a failing test that the dial-chain behavior (strategy walk, Direct timeout/attempt bounds, negative cache consult/record, upstream fallback) is reachable from a new shared package `internal/in/dialchain`; move the existing smart-listener chain tests there unchanged as the red baseline, verify they fail against the not-yet-moved implementation (`go test ./internal/in/dialchain/...`)
- [x] 1.2 Move `smartDial`/`dialDirect`/`hasUpstreamFallback` + negative cache from `internal/in/http/smart.go` into the shared package; keep all moved tests green and the smart listener's own tests untouched and green (`go test ./internal/...`)
- [x] 1.3 Rewire the smart listener to call the shared chain; verify `make test` and `make lint` pass with zero behavior change (parity: `internal/in/http` tests untouched and green)

## 2. Config: types, loader, validation (config-reload delta)

- [x] 2.1 Write failing loader tests: `transparent.listen` empty/absent → disabled, valid host:port parses, malformed value rejected with error naming `transparent.listen`, defaults applied (none — listen is the only field) (`go test ./internal/out/yamlconfig/...`)
- [x] 2.2 Write failing reload-detection tests: changing `transparent.listen`, adding or removing the `transparent` block logs restart-required and does not rebind, mirroring the socks5 case (`go test ./internal/app/...` or the reload test home)
- [x] 2.3 Add `Transparent` struct to `internal/port/types.go`, parsing + validation + reload detection in `internal/out/yamlconfig/loader.go`; make 2.1–2.2 pass; verify `make test` green

## 3. Transparent listener adapter (transparent-listener delta)

- [x] 3.1 Write failing tests for destination recovery: `originaldst_other.go` returns the Linux-required sentinel error; a fake `SO_ORIGINAL_DST` source feeds host:port formatting (IPv4, IPv6-bracketed) into the listener loop (`go test ./internal/in/transparent/...`)
- [x] 3.2 Write failing listener tests: recovered target → `Decide()` strategies walked via the shared dial chain (RU → Direct pinned, default → Upstream, blacklist fallback order); Direct-capable strategy logs a warning naming the destination, Upstream-only does not; all-fail closes the conn; getsockopt failure closes the conn and logs; raw byte relay both directions; no idle timeout on relay (`go test ./internal/in/transparent/...`)
- [x] 3.3 Implement `internal/in/transparent/`: `originaldst_linux.go` (`//go:build linux`, `SO_ORIGINAL_DST` via `golang.org/x/sys/unix` — module moves indirect→direct), `originaldst_other.go` (`//go:build !linux`), accept loop, warning + dial via shared chain, `io.Copy` relay; make 3.1–3.2 pass (`go test ./internal/in/transparent/...`)
- [x] 3.4 Write failing lifecycle tests: binds only when `transparent.listen` non-empty; absent/empty block → process behaves as before; startup on non-Linux with non-empty block fails with an error naming the Linux-only limitation (build-tag-aware test or compile-level check via `go vet` cross-build for darwin/amd64) (`go test ./cmd/...` and a `GOOS=darwin go build ./...` gate task in CI config)
- [x] 3.5 Implement the lifecycle + platform guard in `cmd/http-proxy-local/wire.go` (`bindOptInListeners` gains the fourth listener; `Run` fails on darwin+enabled); make 3.4 pass; verify `GOOS=darwin go build ./...` succeeds and `make test && make lint` green

## 4. Docs and example config

- [x] 4.1 Add a documented `transparent:` block to `config.example.yaml` (opt-in posture, 127.0.0.1:12345 recommended, loop-warning note, restart-required note) and verify the loader accepts the example file as-is (`go test ./internal/out/yamlconfig/ -run ExampleConfig`)
- [x] 4.2 README: transparent-listener section — iptables/ipset stays, redsocks retires, migration steps from design.md, the "never blacklist interception-ipset CIDRs" convention; `docs/architecture.md`: fourth inbound adapter on the shared upstream cascade; verify both render consistently with existing sections (manual read-through against adjacent sections)

## 5. Integration verification

- [x] 5.1 Full gate: `make test && make lint && make build` green; `golangci-lint run` zero findings including the build-tagged linux file (`GOOS=linux golangci-lint run`)
- [x] 5.2 End-to-end smoke on this machine: run the built binary with `transparent.listen` enabled, open a raw TCP conn redirected via a local iptables rule in a network namespace (or `socat` + a stub getsockopt test double at the wire level), verify the recovered destination routes Upstream and bytes relay; verify the absent-block path binds nothing (ss shows no extra listener)

## Workflow follow-up

- Deploy on green106 per design.md Migration Plan; verify `curl --noproxy '*' https://api.telegram.org/` → 302 through the transparent listener, then retire redsocks.
- Archive the change after the project's review requirements are satisfied.
