## Why

The Telegram client cannot use an HTTP/HTTPS proxy — it only accepts SOCKS5 or
MTProto. In the operator's environment DPI blocks both direct connections to
Telegram DCs and MTProto (even obfuscated). The operator's working channel is
an HTTPS-CONNECT cascade (`localhost → msk → ams → Telegram DCs`) that already
serves browser traffic via the existing HTTP listener. Telegram needs a local
SOCKS5 entry point that reuses this exact cascade, so its traffic looks
identical to the browser's TLS-encapsulated flow to DPI.

MTProto is explicitly rejected: it is also TCP-only (no help for UDP calls),
its obfuscation is unnecessary inside a loopback + TLS-upstream channel, and it
cannot be cascaded through an HTTP-CONNECT upstream. SOCKS5 CONNECT-only is the
correct fit.

## What Changes

- **Add a SOCKS5 inbound listener** (new `internal/in/socks5/` adapter) that:
  - Speaks RFC 1928 (greeting, method negotiation, CONNECT request, tunneling).
  - Supports method `0x00` (No Authentication Required) only. Method
    negotiation rejects `0x02` (Username/Password); `0xFF` (No Acceptable
    Methods) is sent for any other offered method.
  - Implements **only the CONNECT command** (`0x01`). The BIND (`0x02`) and
    UDP ASSOCIATE (`0x03`) commands SHALL be refused with reply code `0x07`
    (Command not supported). UDP is intentionally unsupported: the configured
    upstream is an HTTPS-CONNECT (TCP-only) tunnel, so UDP would have nowhere
    to go — advertising UDP support would mislead the client and prevent it
    from falling back to TCP relay.
  - Forwards every accepted CONNECT to the **existing** `dialUpstream` /
    `tunnel` path used by the HTTP listener. No new upstream logic, no new
    routing layer — SOCKS5 is a pure protocol adapter over the existing
    cascade.
  - Applies NO PAC, NO RU-geo routing, NO whitelist/blacklist. SOCKS5 tunnels
    whatever host:port the client requests; routing decisions belong to the
    client (Telegram decides which DCs to reach).
- **Add a `socks5` config block** to `config.yaml`:
  - `socks5.listen` — the bind address. There is NO activating default port;
    the listener starts if and only if this field is set to a non-empty value
    (see design D3). The value `127.0.0.1:1080` is the RECOMMENDED setting,
    documented in `config.example.yaml`, but the operator MUST write it (or
    any other non-empty value) to enable the listener.
  - The block is optional: when omitted entirely, or when `listen` is empty,
    the SOCKS5 listener SHALL NOT start. This keeps the binary
    backward-compatible — existing users see no behavior change.
  - The listener binds only to the configured address and carries no client
    auth; this matches the existing localhost-only posture of the HTTP
    listener.
- **Run two listeners concurrently** in the composition root (`wire.go`): the
  existing HTTP/PAC listener and the new SOCKS5 listener (when configured).
  Both share the single live `configPtr` and the single upstream definition.
- **Hot-reload**: changes to `socks5.listen` SHALL require a restart (same
  posture as the HTTP `listen` field — rebinding mid-flight is out of scope).
  Adding/removing the `socks5` block mid-flight SHALL also require a restart;
  the reloader SHALL log this clearly rather than silently ignoring it.

Out of scope for this change:
- SOCKS5 username/password auth (posture is localhost-only; not needed now).
- SOCKS5 BIND and UDP ASSOCIATE commands.
- IPv6 listen addresses for the SOCKS5 listener (forward-compatible later).
- Per-listener upstream overrides (single shared upstream is confirmed).

## Capabilities

### New Capabilities

- `socks5-listener`: A SOCKS5 (RFC 1928) inbound listener that accepts
  CONNECT requests from local clients (e.g. Telegram), negotiates the
  no-auth method, refuses BIND/UDP ASSOCIATE, and tunnels each accepted
  CONNECT through the shared HTTPS-CONNECT upstream cascade. Covers the
  SOCKS5 wire protocol, command support matrix, localhost-only posture,
  opt-in lifecycle (listener only starts when the `socks5` block is present),
  and the deliberate absence of PAC/geo routing.

### Modified Capabilities

- `config-reload`: Add validation and reload semantics for the new `socks5`
  config block. Validation SHALL accept a valid `socks5.listen` host:port or
  an absent block; reject malformed addresses. Reload SHALL detect
  `socks5.listen` changes and `socks5` block add/remove, log that a restart
  is required (mirroring the existing `listen` field behavior), and continue
  serving on the previously bound address.

## Impact

- **New code**:
  - `internal/in/socks5/` — new package: handshake (greeting + method
    negotiation), request parsing, CONNECT command handling, reply framing.
    Reuses `dialUpstream`/`tunnel` from `internal/in/http/` (likely via a
    small extraction into a shared location — TBD in design).
  - `internal/port/types.go` — add `SOCKS5` struct to `Config` (mirrors
    `Upstream`/`Geo`/`PAC`).
  - `cmd/http-proxy-local/wire.go` — start the SOCKS5 listener alongside the
    HTTP listener when configured; coordinate shutdown of both.
- **Config**: `config.example.yaml` gains a documented `socks5` block;
  `internal/out/yamlconfig/loader.go` gains defaults + validation for it.
- **No new dependencies**: SOCKS5 is a trivial RFC 1928 implementation; no
  third-party SOCKS5 library will be introduced. The Go standard library
  suffices.
- **Docs**: `README.md` gains a section on using the SOCKS5 listener with
  Telegram; `docs/architecture.md` notes the second inbound adapter and the
  shared upstream cascade.
- **Threat model**: unchanged. The SOCKS5 listener is localhost-only with no
  auth, exactly like the existing HTTP listener. It does not expose the proxy
  beyond the loopback and inherits the existing "no client auth on localhost"
  posture.
- **Backward compatibility**: full. Operators who do not add a `socks5` block
  experience zero behavior change — no new listener binds, no new log lines,
  no new config validation failures.
