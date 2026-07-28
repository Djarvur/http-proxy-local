# http-proxy-local

[![CI](https://github.com/Djarvur/http-proxy-local/actions/workflows/pr.yml/badge.svg)](https://github.com/Djarvur/http-proxy-local/actions/workflows/pr.yml)

A self-contained local HTTP routing proxy for macOS, Linux, and Windows. It serves a smart
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

## Installation

### Download a binary

Pre-built binaries for Linux, macOS, and Windows (amd64 + arm64 each) are
published on the [Releases page](https://github.com/Djarvur/http-proxy-local/releases/latest).
Download the archive for your platform, extract it, and run:

| Platform | Archive |
|---|---|
| Linux amd64 | `http-proxy-local_<version>_Linux_amd64.tar.gz` |
| Linux arm64 | `http-proxy-local_<version>_Linux_arm64.tar.gz` |
| macOS amd64 | `http-proxy-local_<version>_Darwin_amd64.tar.gz` |
| macOS arm64 (Apple Silicon) | `http-proxy-local_<version>_Darwin_arm64.tar.gz` |
| Windows amd64 | `http-proxy-local_<version>_Windows_amd64.zip` |
| Windows arm64 | `http-proxy-local_<version>_Windows_arm64.zip` |

```sh
# Example (macOS arm64):
curl -L https://github.com/Djarvur/http-proxy-local/releases/latest/download/http-proxy-local_<version>_Darwin_arm64.tar.gz | tar xz
./http-proxy-local --config config.yaml
```

macOS binaries are **unsigned**. On first run Gatekeeper will block them —
right-click → Open (or `xattr -dr com.apple.quarantine /path/to/binary`) to
allow execution. See Apple's docs on opening apps from unidentified developers.

Once built or downloaded, point your OS/browser at the PAC URL
(`http://127.0.0.1:8080/proxy.pac` by default) — see [Quick start](#quick-start)
for OS-specific configuration steps.

### Build from source

Requires Go 1.26+ (or use [mise](https://mise.jdx.dev/) — see Quick start):

```sh
go install github.com/Djarvur/http-proxy-local/cmd/http-proxy-local@latest
```

or clone and build:

```sh
git clone https://github.com/Djarvur/http-proxy-local.git
cd http-proxy-local
make build   # produces ./bin/http-proxy-local
```

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

The same precedence is applied **server-side** by the optional smart HTTP
listener (below) for clients that ignore the PAC — so browsers and PAC-blind
clients reach the same DIRECT/Upstream decisions.

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

## Using the SOCKS5 listener (e.g. for Telegram)

Some clients — most notably the **Telegram** desktop and mobile apps — cannot
use an HTTP/HTTPS proxy and only speak SOCKS5 (or MTProto, which this proxy
does not implement). For those, enable the optional SOCKS5 listener.

The SOCKS5 listener is **opt-in**. It only starts when `socks5.listen` is
non-empty in your config — there is no default port. Add the block and
restart:

```yaml
socks5:
  listen: 127.0.0.1:1080
```

Then point the client at it:

- **Telegram desktop**: Settings → Advanced → Connection type → Use custom
  proxy → Add proxy → SOCKS5, host `127.0.0.1`, port `1080`, no auth.
- **Telegram Android/iOS**: Settings → Data and Storage → Proxy → Add
  Proxy → SOCKS5, same host/port.

Traffic entering the SOCKS5 listener is tunneled through the **same upstream
cascade** as browser traffic, so to DPI it looks identical to your other
proxied traffic. No PAC, RU-geo, or whitelist/blacklist rules are applied to
SOCKS5 connections — the client decides its own destinations (Telegram picks
its DCs), and the proxy just forwards.

### What works, what doesn't

| Feature | Status |
|---|---|
| Messages, media, channels | ✅ Works (TCP) |
| Voice/video calls | ⚠️ Works via TCP relay (Telegram falls back when UDP is unavailable — degraded quality, higher latency) |
| P2P voice/video (direct UDP) | ❌ Not supported — the upstream cascade is TCP-only (`HTTP CONNECT`), so UDP has no transport through it |

If you need true UDP voice/video, that's an **upstream infrastructure**
change (e.g. a WireGuard tunnel or SOCKS5-upstream-with-UDP-support to your
server), not something this local proxy can provide — the limitation is the
channel to the upstream, not the local listener.

### Why not MTProto?

MTProto is also TCP-only (no help for UDP), its obfuscation is pointless
inside a loopback + TLS-upstream channel (DPI never sees the localhost hop),
and it cannot be cascaded through an HTTP-CONNECT upstream. SOCKS5
CONNECT-only reuses the existing cascade directly. See `openspec/changes/
add-socks5-listener/design.md` for the full alternatives analysis.

## Using the smart HTTP listener (e.g. for zcode)

Some clients — notably the **`zcode`** CLI — honour `HTTP_PROXY`/`HTTPS_PROXY`
but **ignore the system PAC** and expose no way to inject one. Without help,
every request they make is forced through the upstream, even to hosts that
are reachable directly (`z.ai`, RU-domestic endpoints, RFC1918 services).

The **smart HTTP listener** solves this: a second HTTP listener on a separate
port that applies the *same* routing precedence as the PAC
(blacklist → whitelist → local/private → RU geo → default) but evaluates it
**server-side**, so PAC-blind clients get the same DIRECT/Upstream decisions
a browser would make.

The smart listener is **opt-in**. It only starts when `smart.listen` is
non-empty:

```yaml
smart:
  listen: 127.0.0.1:8081
  # Optional tuning (defaults shown):
  dial_timeout: 3s           # per-attempt timeout for direct dials
  dial_attempts: 2           # direct retries before falling back to upstream
  negative_cache_ttl: 60s    # how long to remember a direct failure
```

Point the PAC-blind client at it:

```bash
export HTTP_PROXY=http://127.0.0.1:8081
export HTTPS_PROXY=http://127.0.0.1:8081
```

### How it differs from the PAC

The precedence is identical (parity is enforced by a property test that runs
both the PAC and the server-side decision over the same corpus). Execution
differs in one place: the **blacklist** rule emits `[Direct, Upstream]` and
dials them in order — if direct fails (e.g. `z.ai` is briefly unreachable
directly), it falls back to upstream. A failed direct attempt is remembered
for `negative_cache_ttl`, so subsequent requests skip the doomed direct dial
and go straight to upstream. RU-geo Direct has **no** fallback — move an
unreliable RU host to the whitelist if you want it routed via upstream.

The smart listener does **not** serve `/proxy.pac` (PAC-blind clients don't
fetch it, and the PAC's `PROXY` directive would point at the dumb listener
anyway). It resolves hostnames via the system resolver (no application DNS
cache — the system resolver already caches correctly). See the
`add-smart-http-listener` change archive for the full design.

## Hot reload

The config file is watched with fsnotify. **Edit `config.yaml`, save, and
the next `/proxy.pac` fetch reflects the new rules.** No restart needed.

- Changes to `upstream.*`, `routing.*`, `pac.cache_seconds`,
  `geo.refresh_interval`, `geo.retry_interval` apply immediately to
  subsequent requests.
- Changes to `listen` (the bind address) require a restart — the proxy
  logs a `restart required` warning when this happens, since you almost
  certainly want to know your listen address moved.
- **Editor safe-save is supported**: vim, emacs, VS Code, and most modern
  editors write to a temp file and atomically rename over the original
  (rather than truncating + writing in place). The watcher follows the
  rename and reloads from the new file. Tested in
  `TestRuntime_HotReload_EditorSafeSave`.

### How quickly does the browser pick up the change?

Two latencies compound:

1. **Config save → PAC swap on disk:** ~400ms (fsnotify + debounce +
   render). This is the proxy's internal hot-reload, always fast.
2. **PAC swap → browser re-fetches it:** bounded by the PAC's
   `Cache-Control: max-age`. macOS does not re-read the PAC on a fixed
   timer — it honors HTTP cache headers.

The PAC cache TTL is configurable via `pac.cache_seconds` (default 15).
With the default, the browser revalidates within ~15s of an edit. Set
`pac.cache_seconds: 0` to disable caching entirely (the browser
revalidates on every request — useful while iterating on config). See
[`config.example.yaml`](./config.example.yaml) for the full semantics.

If you ever edit the config and the browser doesn't pick it up within
the expected window, toggle **System Settings → Network → Proxies →
Automatic Proxy Configuration** off and on, or run:

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

MIT — see [LICENSE](./LICENSE).
