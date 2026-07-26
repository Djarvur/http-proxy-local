## Why

A personal local HTTP proxy is needed that routes RU-based traffic directly while sending everything else through a configurable upstream proxy. macOS network settings can then point at this single localhost proxy, and a served PAC file lets the browser itself make the per-request RU/DIRECT-vs-upstream decision without the proxy doing per-connection geo lookups. All configuration (upstream, white/black lists, refresh interval) must update live without an app restart, and the binary must be self-contained — including fetching and caching its own geo data.

## What Changes

- Add a Go binary that runs a forward HTTP proxy on a configurable local address (default `127.0.0.1:8080`), cascading every request to a configured upstream.
- Upstream may be HTTP or HTTPS, with optional basic auth (username/password).
- Serve a smart PAC file at `GET /proxy.pac`. The PAC performs all routing decisions client-side (in the browser): RU IPv4 → `DIRECT`, otherwise → `PROXY <listen-addr>`.
- Routing precedence enforced in the PAC, highest first:
  1. **blacklist** match → `DIRECT` (absolute; overrides everything including local-network and whitelist)
  2. **whitelist** match → `PROXY` (overrides local-network and geo)
  3. **local/private network** (RFC1918, loopback, link-local, plain hostnames) → `DIRECT`
  4. **RU IPv4** (binary search over sorted+merged numeric ranges) → `DIRECT`
  5. default → `PROXY`
- Whitelist/blacklist entries match by **suffix** (`*.example.com`, `example.com`) or **CIDR** (`10.0.0.0/8`).
- Built-in fetcher pulls the RU IPv4 delegation list from RIPE on a configurable interval (default 24h), parses + merges it into numeric `[low high]` ranges, and caches it to disk.
- Hot-reload: `config.yaml` is watched (fsnotify); any change is reparsed, validated, and swapped atomically. PAC bytes are regenerated whenever config OR the cached CIDR list changes.
- v1 scope: IPv4 only for geo. IPv6 deferrable, no PAC v6 ranges in v1.

## Capabilities

### New Capabilities
- `proxy-server`: The localhost forward HTTP proxy — listens on a configurable address, serves `GET /proxy.pac`, and cascades all non-PAC requests to the configured upstream (HTTP/HTTPS, optional basic auth).
- `pac-generation`: Renders the PAC document from config + cached RU ranges. Encodes the routing precedence (blacklist > whitelist > local > RU > default-proxy), suffix/CIDR matching, and the binary-search RU lookup. Hot-regenerated on config or cache change.
- `geo-data-fetcher`: Fetches the RIPE delegation list on a configurable interval, parses RU IPv4 prefixes, merges/sorts them into numeric ranges, caches to disk, and triggers PAC regeneration. Blocking on first run; degrades to empty RU list on failure.
- `config-reload`: Watches `~/.config/http-proxy-local/config.yaml` with fsnotify, reparses + validates on change, and atomically swaps the live config. Triggers PAC regeneration when relevant fields change.
- `engineering-conventions`: Cross-cutting engineering standards binding on all capabilities — mandatory TDD (red/green/refactor), strict `golangci-lint` config with zero-findings gate, and `go-ultimate` as the canonical Go skill (superseding go-best-practices, go-project-setup, golang-code-review, use-modern-go).

### Modified Capabilities
<!-- None — greenfield. -->

## Impact

- **New code**: Go binary, single repo. Packages likely: `cmd/` (entrypoint), `internal/config`, `internal/proxy`, `internal/pac`, `internal/geo`, `internal/reload`.
- **Dependencies**: `gopkg.in/yaml.v3` (config), `github.com/fsnotify/fsnotify` (hot reload), plus Go stdlib `net/http`.
- **Filesystem**: reads `~/.config/http-proxy-local/config.yaml`; writes `~/.config/http-proxy-local/ru-cidrs.cache`.
- **Network**: outbound HTTPS to `ftp.ripe.net` (configurable URL) on refresh; outbound to upstream proxy for all non-PAC traffic.
- **Runtime**: long-running process; suitable for launchd/user agent on macOS.
- **No breaking changes** — greenfield project, no existing consumers.
