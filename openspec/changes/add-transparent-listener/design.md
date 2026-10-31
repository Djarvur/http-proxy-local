# Design

## Context

The smart listener (`internal/in/http/smart.go`) already contains everything
this change needs except the entry point: `Decide()` produces the strategy
list, `smartDial` walks it with Direct/Upstream fallback, negative cache, and
tuning; `bindOptInListeners` in `wire.go` shows the opt-in lifecycle pattern
shared by socks5 and smart. The interception chain on the workstation
(iptables REDIRECT :12345 → redsocks http-connect → 127.0.0.1:8081) exists and
works; only the middle hop is being replaced. See proposal.md for motivation.

## Goals / Non-Goals

**Goals:**

- New in-adapter `internal/in/transparent/` that recovers the original
  destination from the socket and reuses the existing decision + dial chain
  unchanged.
- Fourth opt-in listener wired in `wire.go` alongside the existing three.
- Retire redsocks from the deployment (docs), keeping iptables/ipset/oneshot
  untouched.

**Non-Goals:**

- UDP/TPROXY interception; nftables migration; SO_MARK-based self-exclusion;
  IPv6 interception.
- Changing the routing model, PAC, or the smart listener itself.
- Managing firewall rules from this repo.

## Decisions

### D1: Reuse the smart dial chain, do not copy it

`smartDial`, `dialDirect`, `hasUpstreamFallback` and the negative cache live
in `internal/in/http` (unexported). The transparent adapter needs identical
behavior. Two options:

- **Chosen: extract the dial chain into a shared package**
  (`internal/in/dialchain` or `internal/app/dialchain` — settle at
  implementation; TDD: move tests first, keep them green). `smart.go` keeps
  its HTTP-specific `smartForward` and calls the shared chain from
  `smartHandleConnect`-style code. The transparent adapter calls the same
  shared chain. No behavior change for smart (parity guaranteed by the moved
  tests).
- Alternative: duplicate ~120 lines into `transparent`. Rejected — two
  negative caches, two tuning paths, guaranteed drift.

### D2: Raw TCP relay, no HTTP semantics

The client (zcode bot, MTProto) speaks raw TLS to the original destination.
After the strategy dial the listener splices `client ↔ outbound` with
`io.Copy` both directions (dual-goroutine, or `syscall.Splice` later if
profiling asks). No request parsing, no 502 responses — on all-fail the
connection just closes. CONNECT's "reply 200 first" step does not exist here;
the client never sent anything.

### D3: Destination recovery via getsockopt on the accepted socket

`SO_ORIGINAL_DST` (`SOL_IP=0`, option 80) on the accepted conn. Implementation
split by build tag:

- `originaldst_linux.go` (`//go:build linux`): raw
  `unix.GetsockoptTCPConn`-equivalent via `golang.org/x/sys/unix` — either
  `unix.GetsockoptIPOptions`-style raw call or
  `unix.GetsockoptString`-adjacent; exact call is an implementation detail,
  the contract is: return `host:port` or error. Must handle the listener
  socket family (IPv4 and IPv6-mapped).
- `originaldst_other.go` (`//go:build !linux`): returns a sentinel error
  "transparent interception requires Linux".

`golang.org/x/sys` is already in go.mod (fsnotify dep) — moves from indirect
to direct. No new modules.

### D4: Direct-loop policy — warn, do not block (decision (a) from explore)

If `Decide()` yields a Direct-capable strategy for an intercepted destination,
log a warning naming the destination, then proceed normally. Rationale from
explore: in the real deployment the ipset holds only Telegram DC CIDRs (none
RU), so Direct is unreachable in practice; the loop would only appear through
operator error (blacklisting a DC IP), and a warning makes that visible
without inventing new semantics. Alternatives rejected in explore:
(b) force-Upstream on Direct — silently rewrites blacklist semantics;
(c) dedicated uid + RETURN rule — breaks the user-unit deployment model.

### D5: Recommended port 12345 — redsocks' port

`transparent.listen: 127.0.0.1:12345` keeps the existing iptables rule
(`REDIRECT --to-ports 12345`) valid across the cutover. The loader applies no
default; 12345 is documentation only, same posture as 8081 for smart.

### D6: Platform guard is a startup failure, not silent skip

On non-Linux with a non-empty `transparent.listen`: `Run` returns an error
naming the limitation (before binding anything else, or right after the other
listeners are bound — exact ordering in tasks). Silent skip was rejected: an
operator who wrote the block asked for interception; a silently disabled
feature looks like a broken network. With an empty/absent block on any
platform: no check, no error (mac users keep clean logs).

## Risks / Trade-offs

- [Direct dial loops back through the redirect] → warn per D4; deployment doc
  states the convention: never put interception-ipset CIDRs in the blacklist.
  The ipset content is operator-owned infrastructure, outside this repo.
- [redsocks removal breaks interception if the listener port differs] →
  recommended port equals the current REDIRECT target (D5); migration below
  orders the cutover so the redirect keeps working at every step.
- [getsockopt misuse corrupts connection state] → single well-tested call
  site, build-tagged; failure path closes the conn and logs.
- [Transparent listener binds 127.0.0.1 but firewall redirects non-loopback
  traffic to it] → out of scope: REDIRECT rewrites dst to the local address;
  the listener's bind address must simply match what the rule targets
  (documented).
- [Upstream-fallback strategy hides interception breakage] → same behavior as
  smart today (blacklist case); accepted for parity.

## Migration Plan

On the workstation (green106), after this change ships in a built binary:

1. Build + install the new binary; add `transparent.listen: 127.0.0.1:12345`
   to config.yaml; restart the user unit.
2. Verify: `curl --noproxy '*' https://api.telegram.org/` → 302 (now captured
   by the transparent listener through the SAME :12345 port redsocks used —
   no firewall change yet), `journalctl --user -u http-proxy-local` shows
   transparent handling, not redsocks.
3. `sudo systemctl disable --now redsocks` and remove the package/uid/config.
4. Rollback: re-enable redsocks, empty `transparent.listen`, restart unit —
   firewall rules never changed.

## Open Questions

None — port, Direct policy, and platform guard were settled in explore.
