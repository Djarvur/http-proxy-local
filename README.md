# http-proxy-local

A self-contained local HTTP routing proxy for macOS. It serves a smart
[Proxy Auto-Config (PAC)](https://en.wikipedia.org/wiki/Proxy_auto-config)
file that routes Russian-IP traffic directly and sends everything else
through a configurable upstream proxy. The config file is hot-reloaded —
edit it and the next request reflects the change, no restart needed.

```
                       ┌────────────────────────────────────────────┐
                       │           your machine (localhost)         │
   browser ──PAC──────▶│ http-proxy-local                           │
            ▲          │   ├─ GET /proxy.pac  → smart PAC            │
            │          │   └─ everything else → upstream proxy      │
            │          └─────────────────┬──────────────────────────┘
   macOS Network Settings                  │
   Proxies → Automatic                     ▼
   Configuration URL               configured upstream proxy
   = http://127.0.0.1:8080/proxy.pac
```

The PAC performs all routing decisions **client-side** (in the browser),
so the proxy itself stays simple and HTTPS works without certificate
shenanigans.

## Routing precedence

The PAC evaluates rules top-down; first match wins:

| # | Rule | Result | Notes |
|---|---|---|---|
| 1 | `blacklist` match | `DIRECT` | Absolute veto; overrides everything |
| 2 | `whitelist` match | `PROXY` | Forces upstream even for local addresses |
| 3 | local/private network | `DIRECT` | RFC1918, loopback, link-local, plain hostnames |
| 4 | Russian IPv4 | `DIRECT` | Binary search over merged RIPE delegation ranges |
| 5 | default | `PROXY` | Everything else goes through the upstream |

Both `whitelist` and `blacklist` accept suffix patterns (`*.example.com`)
and CIDR ranges (`10.0.0.0/8`).

## Quick start

### Prerequisites

- Go 1.26+ (or just use [mise](https://mise.jdx.dev/) — see below)
- macOS (the PAC URL is pointed at via System Settings → Network)

### Install mise tooling (recommended)

[mise](https://mise.jdx.dev/) pins the exact Go and `golangci-lint` versions
so the build is reproducible:

```bash
mise install   # reads mise.toml, installs go 1.26.5 + golangci-lint 2.12.2
```

### Build

```bash
mise run build         # → bin/http-proxy-local
# or, without mise:
go build -o bin/http-proxy-local ./cmd/http-proxy-local
```

### Configure

Copy the example config and edit:

```bash
mkdir -p ~/.config/http-proxy-local
cp config.example.yaml ~/.config/http-proxy-local/config.yaml
$EDITOR ~/.config/http-proxy-local/config.yaml
```

See [`config.example.yaml`](./config.example.yaml) for every field and its
default.

### Run

```bash
mise run run           # builds + runs with the default config path
# or:
./bin/http-proxy-local [-config /path/to/config.yaml]
```

On first run, the binary blocks for a few seconds while it fetches the RIPE
delegation list (one-time, cached afterward). The PAC is served at
`http://127.0.0.1:8080/proxy.pac` by default.

### Point macOS at it

1. **System Settings → Network → (your interface) → Details → Proxies**
2. Enable **Automatic Proxy Configuration**
3. URL: `http://127.0.0.1:8080/proxy.pac`
4. Apply. Browsers pick up the PAC within seconds.

To verify, visit a RU site (e.g. `https://ya.ru`) — it should load normally
via direct connection. A non-RU site (e.g. `https://example.com`) routes
through your configured upstream.

## Hot reload

The config file is watched with fsnotify. **Edit `config.yaml`, save, and
the next `/proxy.pac` fetch reflects the new rules.** No restart needed.

- Changes to `upstream.*`, `routing.*`, `geo.refresh_interval`,
  `geo.retry_interval` apply immediately to subsequent requests.
- Changes to `listen` (the bind address) require a restart — the proxy
  logs a `restart required` warning when this happens, since you almost
  certainly want to know your listen address moved.
- **Editor safe-save is supported**: vim, emacs, VS Code, and most modern
  editors write to a temp file and atomically rename over the original
  (rather than truncating + writing in place). The watcher follows the
  rename and reloads from the new file. Tested in
  `TestRuntime_HotReload_EditorSafeSave`.

### How quickly does the browser pick up the change?

End-to-end latency is ~400ms from config save to PAC swap on disk. **The
browser sees the change on its next PAC fetch** — macOS does not re-read
the PAC on a fixed timer; it honors HTTP cache headers, and we serve the
PAC with `Cache-Control: no-cache` so the system revalidates on every
request. Without that header, browsers heuristically cache the PAC for
up to ~2 hours, which would silently defeat hot-reload.

If you ever edit the config and the browser doesn't pick it up within a
few seconds, toggle **System Settings → Network → Proxies → Automatic
Proxy Configuration** off and on, or run:

```bash
networksetup -setautoproxyurl "Wi-Fi" "http://127.0.0.1:8080/proxy.pac"
```

## RU geo data

The binary is self-contained: it fetches the RIPE delegation list from
`https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest` on a
configurable interval (default: every 24h), parses + merges the ~6000 RU
IPv4 prefixes into ~3000-4000 disjoint ranges, and caches them to
`~/.config/http-proxy-local/ru-cidrs.cache`.

- Successful refresh → wait `geo.refresh_interval` (default `24h`).
- Failed refresh → wait `geo.retry_interval` (default `5m`), retry until
  success, then revert to the success interval.
- Fresh cache on startup → no fetch, instant serve.
- Stale cache on startup → blocking fetch, falls back to stale on failure.
- No cache + fetch fails on first run → serves an empty-RU PAC (all
  non-listed hosts route to upstream) and keeps retrying. Never crashes.

## Testing

```bash
mise run test         # full unit + integration suite
mise run lint         # strict golangci-lint (zero findings required)
mise run test-race    # with the race detector
```

The PAC routing rules are verified end-to-end by a node-based execution
harness (`internal/app/pacjs_test.go`) that loads the rendered PAC and
asserts on `FindProxyForURL` for every precedence scenario.

### Real-proxy smoke tests (opt-in)

Tests against a real upstream proxy are gated behind a build tag and
excluded from the default suite (they need network + credentials):

```bash
ZCODE_HTTP_PROXY=https://user:pass@host:port \
  go test -tags realproxy -run TestRealProxy ./internal/in/http/
```

Credentials are read from the environment, never hardcoded. See
[`CONTRIBUTING.md`](./CONTRIBUTING.md) for details.

## Architecture

See [`docs/architecture.md`](./docs/architecture.md) for the design
rationale, package layout, and the hot-reload contract. Short version:

- **PAC-decides-routing**: the browser evaluates the PAC and decides
  DIRECT vs PROXY per URL. The Go binary is a thin PAC generator +
  cascading forwarder.
- **Hexagonal layout** (per `go-ultimate`): ports and adapters; the app
  layer is pure logic with no I/O.
- **Atomic snapshots**: live config and PAC bytes are read via
  `atomic.Pointer` on every request, lock-free on the hot path.

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). The short version: TDD is
mandatory, the strict lint gate must pass with zero findings, and Go
architecture questions route through the `go-ultimate` skill.

## Project layout

```
http-proxy-local/
├── go.mod                         module github.com/Djarvur/http-proxy-local
├── .golangci.yml                  strict lint config (17 linters)
├── mise.toml                      pins go + golangci-lint versions
├── Makefile                       build/test/lint/run targets
├── cmd/http-proxy-local/          package main: entrypoint + wire.go
├── internal/
│   ├── port/                      boundary types & Out-port interfaces
│   ├── app/                       pure logic: PAC render, RIPE parse, fetcher, orchestrator
│   ├── in/http/                   inbound adapter: PAC handler + forwarder + CONNECT
│   └── out/{fscache,fsnotify,ripe,yamlconfig}/   outbound adapters
├── .github/workflows/             pr.yml (gate) + periodic.yml (weekly health)
├── docs/architecture.md           design rationale
└── openspec/                      change history (proposal/specs/design/tasks)
```

## License

(Add license information here.)
