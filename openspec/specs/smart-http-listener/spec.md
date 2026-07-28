# smart-http-listener Specification

## Purpose

A second HTTP inbound listener, bound to a separate port from the dumb
HTTP/PAC listener, that applies the same routing precedence as the PAC
(blacklist → whitelist → local/private → RU geo → default) but evaluates it
*server-side* per request. Intended for clients that ignore the system PAC and
expose no way to inject one (notably the `zcode` CLI), so their traffic gets
the same DIRECT-vs-upstream decisions a browser would make via the PAC.

The capability covers: the precedence mapping from host + resolved IPs to an
ordered Direct/Upstream strategy list; DNS resolution via the system resolver;
RU-IP pinning for the CONNECT command; the negative cache that suppresses
repeated Direct-dial failures; the configurable Direct-dial timeout and
attempt count; the opt-in lifecycle (listener only starts when `smart.listen`
is set); and the deliberate non-serving of `/proxy.pac`.

The capability does NOT cover: applying routing to the SOCKS5 listener,
refactoring the PAC renderer into a shared routing model, an application-level
DNS cache, IP-pinning for plain-HTTP forward requests, IPv6 geo, per-listener
upstream overrides, TLS on the smart listener, or client auth. These are
explicitly out of scope.

## Requirements

### Requirement: Apply the PAC-equivalent routing precedence server-side

For each incoming request (plain HTTP or CONNECT), the listener SHALL evaluate
the routing precedence in the same order the PAC uses, and SHALL produce an
ordered list of dial strategies. The precedence, first match wins:

1. `blacklist` match → `[Direct, Upstream]` (direct first, upstream fallback).
2. `whitelist` match → `[Upstream]`.
3. local/private network (plain hostname, or an IP in RFC1918/loopback/
   link-local) → `[Direct]`.
4. RU IPv4 (any resolved IP falls in a RU range) → `[Direct]`, pinned to the
   matched RU IP.
5. default → `[Upstream]`.

A host that matches no earlier rule falls through to the default. Blacklist
and whitelist entries match by suffix (`*.example.com`) or by CIDR
(`10.0.0.0/8`); CIDR matching requires DNS resolution (the hostname must be
resolved to an IP before the CIDR can be tested). The listener SHALL read the
live `routing.whitelist`, `routing.blacklist`, and `port.RURanges` per request,
so hot-reloaded rules apply to the next request with no restart.

#### Scenario: Blacklist match yields Direct with Upstream fallback

- **WHEN** a request arrives for a host that matches a `blacklist` entry
- **THEN** the listener SHALL produce the strategy list `[Direct, Upstream]`
- **AND** SHALL attempt the Direct strategy first
- **AND** SHALL fall back to the Upstream strategy only if the Direct strategy
  fails after the configured `dial_attempts` with the configured `dial_timeout`

#### Scenario: Whitelist match yields Upstream only

- **WHEN** a request arrives for a host that matches a `whitelist` entry
- **THEN** the listener SHALL produce the strategy list `[Upstream]`
- **AND** SHALL NOT attempt a Direct connection

#### Scenario: Plain hostname yields Direct only

- **WHEN** a request arrives for a host that is a plain hostname (no dots, per
  `isPlainHostName` semantics)
- **THEN** the listener SHALL produce the strategy list `[Direct]`
- **AND** SHALL NOT consult DNS or attempt the upstream

#### Scenario: Private-IP host yields Direct only

- **WHEN** a request arrives for a host whose resolved IP is in `10.0.0.0/8`,
  `172.16.0.0/12`, `192.168.0.0/16`, `127.0.0.0/8`, or `169.254.0.0/16`
- **THEN** the listener SHALL produce the strategy list `[Direct]`
- **AND** SHALL NOT attempt the upstream

#### Scenario: RU IPv4 yields Direct pinned to the RU IP

- **WHEN** a request arrives for a host whose resolved IPs include at least one
  IPv4 in a RU range
- **THEN** the listener SHALL produce the strategy list `[Direct]`
- **AND** the Direct strategy SHALL dial the first resolved IPv4 that falls in
  a RU range (not the hostname)
- **AND** SHALL NOT fall back to the upstream if the Direct dial fails

#### Scenario: Non-matching host yields Upstream only

- **WHEN** a request arrives for a host that matches no blacklist, whitelist,
  local-network, or RU-geo rule
- **THEN** the listener SHALL produce the strategy list `[Upstream]`

#### Scenario: Blacklist overrides whitelist, local, and geo

- **WHEN** a host is in both the `blacklist` and the `whitelist`
- **THEN** the listener SHALL produce the blacklist strategy list `[Direct, Upstream]`
- **AND** SHALL NOT consult the whitelist rule

### Requirement: Resolve target hostnames via the system resolver

For any request whose precedence evaluation requires an IP (CIDR matching, the
RU-geo rule), the listener SHALL resolve the request's target hostname by
calling the system resolver (`net.Resolver.LookupIPAddr` or equivalent). The
listener SHALL NOT introduce an application-level DNS cache; it SHALL rely on
the system resolver's own caching, negative-answer handling, and `/etc/hosts`
processing. The listener SHALL resolve only when an IP is needed — rules that
match by hostname suffix (blacklist/whitelist) or by plain-hostname check
SHALL NOT trigger resolution.

#### Scenario: Resolution triggered for CIDR rule

- **WHEN** a request arrives whose hostname does not match any suffix rule and
  is not a plain hostname, and the precedence reaches the CIDR or geo check
- **THEN** the listener SHALL resolve the hostname via the system resolver
- **AND** SHALL use the resolved IPs to evaluate the CIDR and geo rules

#### Scenario: No resolution for suffix match

- **WHEN** a request arrives whose hostname matches a `blacklist` or
  `whitelist` suffix entry
- **THEN** the listener SHALL apply the match without resolving the hostname

#### Scenario: Resolution failure falls through to default

- **WHEN** the system resolver returns no IPs or an error for a hostname that
  reached the CIDR/geo check
- **THEN** the listener SHALL treat no rule as matching
- **AND** SHALL fall through to the default `[Upstream]` strategy list

### Requirement: Walk the strategy list in order, first success wins

After producing the ordered strategy list, the listener SHALL attempt each
strategy in order until one establishes a usable connection. On success the
listener SHALL proceed with the request (tunnel for CONNECT, forward for plain
HTTP) using that connection. If a strategy fails, the listener SHALL move to
the next strategy. If all strategies fail, the listener SHALL return an error
response (502 Bad Gateway for plain HTTP, connection close for CONNECT) and
SHALL log the failure (errors-only logging; no per-request success logging).

#### Scenario: First strategy succeeds

- **WHEN** the first strategy in the list establishes a connection
- **THEN** the listener SHALL use that connection
- **AND** SHALL NOT attempt any remaining strategy

#### Scenario: Fallback after first strategy fails

- **WHEN** the first strategy is `[Direct, Upstream]` and the Direct dial fails
  within the configured `dial_attempts` and `dial_timeout`
- **THEN** the listener SHALL attempt the Upstream strategy
- **AND** on success SHALL use the upstream connection

#### Scenario: All strategies fail

- **WHEN** every strategy in the list fails
- **THEN** the listener SHALL return an error to the client
- **AND** SHALL log the failure

### Requirement: Apply a negative cache to failed Direct attempts

When a Direct strategy fails (the host was unreachable directly), the listener
SHALL record the hostname in a negative cache with an expiry of
`smart.negative_cache_ttl`. Before attempting any Direct strategy, the listener
SHALL consult the negative cache; if the hostname is still within its
negative-TTL window, the listener SHALL skip the Direct attempt and move
directly to the next strategy in the list. The negative cache SHALL be keyed
by hostname (host-level unavailability, not per-port). The negative cache
SHALL be consulted ONLY on Direct strategies; Upstream strategies SHALL NOT
consult it.

#### Scenario: Failed Direct dial marks the hostname negative

- **WHEN** a Direct dial fails for hostname `z.ai`
- **THEN** the listener SHALL record `z.ai` in the negative cache with the
  configured TTL
- **AND** the next request for `z.ai` within the TTL SHALL skip the Direct
  attempt and go straight to the next strategy

#### Scenario: Negative entry expires

- **WHEN** a hostname's negative-cache entry has passed its expiry
- **THEN** the listener SHALL retry the Direct strategy on the next request
- **AND** on success SHALL effectively clear the entry for that hostname

#### Scenario: Negative cache not consulted for Upstream

- **WHEN** a strategy list contains only `[Upstream]` (e.g. a whitelist match)
- **THEN** the listener SHALL NOT consult the negative cache

### Requirement: Bound Direct-dial attempts with configurable timeout and count

Each Direct dial attempt SHALL be bounded by `smart.dial_timeout` (default
`3s`). The listener SHALL make up to `smart.dial_attempts` (default `2`)
Direct attempts before considering the Direct strategy failed. Both values
SHALL be read from the live config per request, so hot-reloaded values apply
to the next request with no restart.

#### Scenario: Direct attempt times out

- **WHEN** a Direct dial does not complete within `dial_timeout`
- **THEN** the listener SHALL abort that attempt
- **AND** SHALL count it against `dial_attempts`

#### Scenario: Multiple Direct attempts before fallback

- **WHEN** `dial_attempts` is greater than 1 and the first Direct attempt fails
- **THEN** the listener SHALL retry the Direct dial up to `dial_attempts` times
- **AND** only after all attempts fail SHALL it record the negative cache entry
  and move to the next strategy

### Requirement: Tunnel accepted CONNECT requests with strategy-driven dialing

For a CONNECT request, the listener SHALL hijack the client connection,
evaluate the precedence for the target host, walk the strategy list to
establish a connection, and on success SHALL reply `HTTP/1.1 200 Connection
Established` and tunnel bytes bidirectionally between client and the
established connection until either side closes. For the RU-geo Direct
strategy, the listener SHALL dial the matched RU IP directly (not the
hostname). For other Direct strategies, the listener SHALL dial by hostname.
For Upstream strategies, the listener SHALL dial via `cascade.Dial` with the
configured upstream.

#### Scenario: CONNECT with blacklist fallback succeeds via Direct

- **WHEN** a CONNECT arrives for a blacklisted host and the Direct dial
  succeeds
- **THEN** the listener SHALL reply `200 Connection Established`
- **AND** SHALL tunnel bytes through the Direct connection

#### Scenario: CONNECT with blacklist fallback succeeds via Upstream

- **WHEN** a CONNECT arrives for a blacklisted host, the Direct dial fails, and
  the Upstream dial succeeds
- **THEN** the listener SHALL reply `200 Connection Established`
- **AND** SHALL tunnel bytes through the upstream connection

#### Scenario: CONNECT to a RU host dials the RU IP

- **WHEN** a CONNECT arrives for a host whose resolved IPs include a RU IPv4
- **THEN** the listener SHALL dial the first RU IPv4 directly (not the hostname)
- **AND** SHALL reply `200 Connection Established` on success
- **AND** SHALL NOT fall back to the upstream if the RU-IP dial fails

### Requirement: Forward plain-HTTP requests with strategy-driven upstream selection

For a plain-HTTP request (absolute-URI, non-CONNECT), the listener SHALL
evaluate the precedence for the target host, select the first successful
strategy, and forward the request accordingly. For the Direct strategy the
listener SHALL build a transport that dials the target directly (by hostname;
no IP-pinning for plain HTTP). For the Upstream strategy the listener SHALL
build a transport that proxies through the configured upstream (reusing the
dumb listener's `buildTransport` logic). Responses SHALL be relayed verbatim
(headers and body).

#### Scenario: Plain HTTP via Direct

- **WHEN** a plain-HTTP request arrives for a host whose first successful
  strategy is Direct
- **THEN** the listener SHALL forward the request directly to the target
- **AND** SHALL relay the response to the client

#### Scenario: Plain HTTP via Upstream

- **WHEN** a plain-HTTP request arrives for a host whose first successful
  strategy is Upstream
- **THEN** the listener SHALL forward the request through the configured upstream
- **AND** SHALL relay the response to the client

#### Scenario: Plain HTTP without IP-pinning

- **WHEN** a plain-HTTP request arrives for a RU host (resolved to a RU IP)
- **THEN** the listener SHALL dial by hostname (NOT by the RU IP)
- **AND** the system resolver selects the IP

### Requirement: Do not serve the PAC from the smart listener

The listener SHALL NOT serve `/proxy.pac` (or any path) as a PAC document.
A GET request to `/proxy.pac` on the smart listener SHALL receive a 404
response. The listener SHALL NOT share the dumb listener's PAC handler.

#### Scenario: PAC request rejected

- **WHEN** a client sends a GET request for `/proxy.pac` to the smart listener
- **THEN** the listener SHALL respond with HTTP 404
- **AND** SHALL NOT return PAC content

### Requirement: Start only when the smart config block is enabled

The smart listener SHALL start if and only if `smart.listen` is non-empty at
process startup. When `smart.listen` is empty (whether because the `smart`
block is absent or because the field is explicitly set to empty), the listener
SHALL NOT bind a port and the process SHALL behave identically to a build
without this feature (full backward compatibility). The listener SHALL bind to
the configured address at startup; subsequent changes to `smart.listen` via
hot-reload SHALL require a process restart and SHALL log an error explaining
this.

#### Scenario: Listener starts when configured

- **WHEN** the configuration sets `smart.listen: 127.0.0.1:8081`
- **THEN** the listener SHALL bind to `127.0.0.1:8081` at process startup
- **AND** SHALL accept connections

#### Scenario: Listener does not start when block absent

- **WHEN** the configuration omits the `smart` block entirely
- **THEN** the listener SHALL NOT bind any port
- **AND** the process SHALL behave identically to a build without this feature

#### Scenario: Listener does not start when listen is empty

- **WHEN** the configuration sets `smart.listen: ""` (explicit opt-out)
- **THEN** the listener SHALL NOT bind any port

#### Scenario: listen change requires restart

- **WHEN** the `smart.listen` field is edited in config while the server is
  running
- **THEN** the system SHALL log an error stating that a restart is required to
  apply the new smart listen address
- **AND** SHALL NOT rebind the listener mid-flight

#### Scenario: smart block added or removed requires restart

- **WHEN** the `smart` block is added to (or removed from) config while the
  server is running
- **THEN** the system SHALL log an error stating that a restart is required
- **AND** SHALL NOT start or stop the listener mid-flight

### Requirement: Carry no client authentication and bind only the configured address

The listener SHALL NOT support client authentication — it inherits the
localhost-only, no-client-auth posture of the existing HTTP and SOCKS5
listeners. The listener SHALL bind only to the address the operator explicitly
wrote in `smart.listen`; there is NO activating default port. The listener
SHALL NOT bind to wildcard interfaces unless the operator explicitly configures
one. The value `127.0.0.1:8081` is the RECOMMENDED value documented in
`config.example.yaml`, but it is not applied by the config loader — the
operator MUST write it (or any other non-empty value) to enable the listener.

#### Scenario: No authentication supported

- **WHEN** a client connects to the smart listener
- **THEN** the listener SHALL NOT require or accept any client authentication
- **AND** SHALL process the request as-is

#### Scenario: Custom listen address

- **WHEN** the configuration sets `smart.listen: 127.0.0.1:8082`
- **THEN** the listener SHALL bind to `127.0.0.1:8082`

#### Scenario: No activating default port

- **WHEN** the `smart` block is present but `smart.listen` is omitted
- **THEN** the listener SHALL NOT bind any port
- **AND** SHALL be treated as disabled (operator must write a non-empty
  `smart.listen` to enable)
