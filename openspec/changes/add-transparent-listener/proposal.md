# Proposal: add-transparent-listener

## Why

Proxy-blind clients (zcode Telegram bot long-poll, any raw TCP to blocked DCs)
are intercepted today by an external redsocks daemon chained into the local
iptables REDIRECT. The Ubuntu redsocks build is quirk-limited (no
`https-connect`), runs as a separate daemon with its own uid and config file,
and duplicates work http-proxy-local already does. Embedding a transparent
(redirect-capture) listener removes the daemon while keeping the kernel-side
redirect untouched.

## What Changes

- **Add a transparent inbound listener** (new `internal/in/transparent/`
  adapter) that:
  - Accepts TCP connections redirected by the local firewall
    (iptables/nftables REDIRECT or TPROXY-adjacent flows via REDIRECT).
  - Recovers the **original destination** address via
    `getsockopt(SOL_IP, SO_ORIGINAL_DST)` — Linux-only; guarded by a `linux`
    build tag.
  - Applies the **same server-side routing precedence as the smart listener**
    (blacklist → whitelist → local/private → RU geo → default) to the
    recovered host:port, and walks the resulting strategy list (Direct /
    Upstream) with the same dial chain (timeout, attempts, negative cache).
  - Relays raw TCP bytes bidirectionally after the strategy dial — no wire
    protocol, no HTTP parsing; the client believes it is talking to the
    original destination directly.
  - Logs a **warning** when a redirected destination resolves to a Direct
    strategy (loop-risk visibility: a Direct dial back into an intercepted
    CIDR would re-enter the redirect; prevented today by convention — DC CIDRs
    must never be blacklisted — per decision (a), not by mechanism).
  - Applies NO client authentication, matching the localhost-only posture of
    the other listeners.
- **Add a `transparent` config block**: `transparent.listen` only. Opt-in with
  NO activating default port, same posture as `socks5`/`smart`. Recommended
  value `127.0.0.1:12345` — the port redsocks used, so the existing iptables
  `REDIRECT --to-ports 12345` rule needs no change at cutover.
- **Hot-reload**: changes to `transparent.listen` (address change, block
  add/remove) require a restart, mirroring `socks5.listen`/`smart.listen`.
- **Startup failure on unsupported platforms**: when `transparent.listen` is
  non-empty on non-Linux (macOS builds), the process SHALL fail startup with a
  clear error instead of silently ignoring the block.
- **Deployment**: redsocks daemon, its uid, and `/etc/redsocks.conf` are
  retired on the workstation; iptables + ipset + oneshot service stay as-is.
  Documented in README (deployment section), not automated by this repo.

Out of scope:
- UDP interception (TPROXY) — the upstream cascade is TCP-only; unchanged from
  the redsocks status quo.
- nftables migration, TPROXY, SO_MARK-based self-exclusion.
- IPv6 interception.

## Capabilities

### New Capabilities

- `transparent-listener`: A Linux-only inbound listener that captures
  firewall-redirected TCP connections, recovers the original destination via
  `SO_ORIGINAL_DST`, applies the shared server-side routing precedence and
  dial chain, and relays the stream raw. Covers destination recovery, strategy
  reuse, the Direct-loop warning, opt-in lifecycle, platform guard, and the
  no-auth localhost posture.

### Modified Capabilities

- `config-reload`: Add validation and reload semantics for the new
  `transparent` config block. Validation SHALL accept a valid
  `transparent.listen` host:port or an absent block; reject malformed
  addresses. Reload SHALL detect `transparent.listen` changes and block
  add/remove and log that a restart is required (mirroring
  `socks5.listen`/`smart.listen`).

## Impact

- **New code**: `internal/in/transparent/` — listener, `SO_ORIGINAL_DST`
  retrieval (build-tagged `linux` file), raw relay. Reuses the shared routing
  decision and dial chain already extracted for the smart listener.
- **Config**: `internal/port/types.go` gains `Transparent` struct;
  `internal/out/yamlconfig/loader.go` gains defaults + validation;
  `config.example.yaml` gains a documented block.
- **Composition**: `cmd/http-proxy-local/wire.go` starts the transparent
  listener alongside HTTP/SOCKS5/smart when configured.
- **Dependencies**: `golang.org/x/sys` moves from indirect to direct (already
  in go.mod for fsnotify) — used for the raw getsockopt. No new modules.
- **Docs**: README gains a transparent-listener section (iptables/ipset
  wiring, redsocks retirement, DC-CIDR-not-blacklisted rule);
  `docs/architecture.md` notes the fourth inbound adapter.
- **Backward compatibility**: full. Operators without a `transparent` block
  see zero behavior change — no bind, no new log lines, no validation change.
- **Threat model**: unchanged — localhost-only, no auth, same as existing
  listeners. The listener accepts only connections the local firewall
  redirected to it.
