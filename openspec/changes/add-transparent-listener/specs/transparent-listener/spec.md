# Spec Delta

## Purpose

A Linux-only inbound listener that captures firewall-redirected TCP
connections (iptables REDIRECT), recovers the original destination address via
`SO_ORIGINAL_DST`, applies the same server-side routing precedence and dial
chain as the smart listener, and relays the TCP stream raw. Replaces the
external redsocks daemon in the transparent-interception chain.

## ADDED Requirements

### Requirement: Recover the original destination of redirected connections

The listener SHALL accept TCP connections on the configured `transparent.listen`
address and, for each accepted connection, SHALL recover the pre-redirect
destination address by querying the socket option `SO_ORIGINAL_DST` on the
accepted socket. The recovered address SHALL be formatted as `host:port`
(IPv6 bracketed) and used as the effective request target for all downstream
routing. The listener SHALL NOT require any protocol handshake from the client.

#### Scenario: Original destination recovered

- **WHEN** the firewall redirects a TCP connection to
  `127.0.0.1:149.154.167.220:443` (dst port `transparent.listen`) and the
  listener accepts it
- **THEN** the listener SHALL recover `149.154.167.220:443` as the effective
  request target via `SO_ORIGINAL_DST`
- **AND** SHALL proceed to route the connection to that target

#### Scenario: getsockopt fails

- **WHEN** `SO_ORIGINAL_DST` retrieval fails on an accepted connection
  (e.g. a client connected directly, not via REDIRECT)
- **THEN** the listener SHALL close the connection
- **AND** SHALL log the failure (errors-only logging)

### Requirement: Apply the smart-listener routing precedence to recovered destinations

For each recovered `host:port`, the listener SHALL evaluate the same routing
precedence the smart listener uses, in the same order, producing the same
ordered strategy lists: blacklist match → `[Direct, Upstream]`; whitelist
match → `[Upstream]`; local/private → `[Direct]`; RU IPv4 → `[Direct]` pinned
to the matched RU IP; default → `[Upstream]`. The listener SHALL read the live
config per connection, so hot-reloaded rules apply immediately.

#### Scenario: Intercepted RU DC destination routes Direct

- **WHEN** a redirected connection targets an IP inside a RU range and no
  whitelist or blacklist entry matches
- **THEN** the listener SHALL produce the strategy list `[Direct]` pinned to
  the matched RU IP

#### Scenario: Intercepted non-RU destination routes Upstream

- **WHEN** a redirected connection targets a host that matches no routing rule
- **THEN** the listener SHALL produce the strategy list `[Upstream]`

#### Scenario: Whitelisted destination routes Upstream only

- **WHEN** a redirected connection targets a host matching a whitelist entry
- **THEN** the listener SHALL produce the strategy list `[Upstream]`
- **AND** SHALL NOT attempt a Direct connection

### Requirement: Walk the strategy list with the shared dial chain

The listener SHALL attempt strategies in order using the same dial chain as
the smart listener: Direct dials bounded by `dial_timeout` and
`dial_attempts`, Upstream dials via the configured cascade, negative-cache
consultation before Direct attempts and recording after Direct failure, first
success wins, all-fail closes the connection and logs. The negative cache,
timeout, and attempt values SHALL be read from the live config per connection.

#### Scenario: Direct first, upstream fallback

- **WHEN** the strategy list is `[Direct, Upstream]` and the Direct dial fails
  after `dial_attempts` attempts
- **THEN** the listener SHALL attempt the Upstream strategy
- **AND** on success SHALL relay through the upstream connection

#### Scenario: Negative cache suppresses repeated Direct dials

- **WHEN** a Direct dial failed for the destination host within
  `negative_cache_ttl`
- **THEN** the next redirected connection to that host SHALL skip the Direct
  attempt

#### Scenario: All strategies fail

- **WHEN** every strategy in the list fails
- **THEN** the listener SHALL close the client connection
- **AND** SHALL log the failure

### Requirement: Relay the TCP stream raw without protocol overhead

After a strategy establishes the outbound connection, the listener SHALL
relay bytes bidirectionally between the client connection and the outbound
connection until either side closes, without parsing, modifying, or
framing the stream. The listener SHALL NOT impose an idle timeout on the
relay phase — long-lived connections (e.g. MTProto long-poll) MAY stay open
indefinitely.

#### Scenario: Raw byte relay

- **WHEN** the outbound connection is established
- **THEN** the listener SHALL copy bytes client→outbound and outbound→client
  verbatim until either side closes

#### Scenario: Long-lived tunnel preserved

- **WHEN** client and outbound exchange no bytes for an extended period
- **THEN** the listener SHALL keep both connections open
- **AND** SHALL NOT sever the connection on idle timeout

### Requirement: Warn when routing resolves a redirected connection to Direct

When the routing precedence resolves a redirected connection to a strategy
list containing Direct, the listener SHALL log a warning naming the
destination before dialing. The warning SHALL NOT block or alter routing —
the connection proceeds per the normal strategy list. This surfaces
violations of the deployment convention that destinations in the
interception ipset MUST NOT be blacklisted: a Direct dial to such a CIDR
would loop back through the firewall redirect.

#### Scenario: Blacklisted destination warns

- **WHEN** a redirected connection targets a host matching a blacklist entry
- **THEN** the listener SHALL log a warning naming the destination
- **AND** SHALL proceed with the `[Direct, Upstream]` strategy list normally

#### Scenario: RU destination warns

- **WHEN** a redirected connection targets a RU IPv4
- **THEN** the listener SHALL log a warning naming the destination
- **AND** SHALL dial the matched RU IP as usual

#### Scenario: Upstream-only routing does not warn

- **WHEN** a redirected connection resolves to `[Upstream]` only
  (whitelist or default)
- **THEN** the listener SHALL NOT log a loop-risk warning

### Requirement: Start only when the transparent config block is enabled

The transparent listener SHALL start if and only if `transparent.listen` is
non-empty at process startup. When `transparent.listen` is empty (block absent
or field empty), the listener SHALL NOT bind a port and the process SHALL
behave identically to a build without this feature (full backward
compatibility). The listener SHALL bind at startup; subsequent changes to
`transparent.listen` via hot-reload SHALL require a process restart and SHALL
log an error explaining this.

#### Scenario: Listener starts when configured

- **WHEN** the configuration sets `transparent.listen: 127.0.0.1:12345`
- **THEN** the listener SHALL bind to `127.0.0.1:12345` at process startup
- **AND** SHALL accept redirected connections

#### Scenario: Listener does not start when block absent

- **WHEN** the configuration omits the `transparent` block entirely
- **THEN** the listener SHALL NOT bind any port
- **AND** the process SHALL behave identically to a build without this feature

#### Scenario: Listener does not start when listen is empty

- **WHEN** the configuration sets `transparent.listen: ""` (explicit opt-out)
- **THEN** the listener SHALL NOT bind any port

#### Scenario: listen change requires restart

- **WHEN** the `transparent.listen` field is edited in config while the server
  is running
- **THEN** the system SHALL log an error stating that a restart is required
- **AND** SHALL NOT rebind the listener mid-flight

#### Scenario: transparent block added or removed requires restart

- **WHEN** the `transparent` block is added to (or removed from) config while
  the server is running
- **THEN** the system SHALL log an error stating that a restart is required
- **AND** SHALL NOT start or stop the listener mid-flight

### Requirement: Refuse startup on unsupported platforms when enabled

The `SO_ORIGINAL_DST` retrieval is Linux-specific. When
`transparent.listen` is non-empty on a non-Linux platform (macOS builds), the
process SHALL fail startup with an error naming the limitation, instead of
starting the listener or silently ignoring the block. When
`transparent.listen` is empty on any platform, the process SHALL start
normally.

#### Scenario: Enabled block on macOS fails startup

- **WHEN** the binary is built for a non-Linux platform and the configuration
  sets a non-empty `transparent.listen`
- **THEN** the process SHALL fail startup with an error naming the
  Linux-only limitation

#### Scenario: Absent block on macOS starts normally

- **WHEN** the binary is built for a non-Linux platform and
  `transparent.listen` is empty
- **THEN** the process SHALL start normally with the listener disabled

### Requirement: Carry no client authentication and bind only the configured address

The listener SHALL NOT support client authentication, matching the
localhost-only, no-client-auth posture of the existing listeners. The listener
SHALL bind only to the address written in `transparent.listen`; there is NO
activating default port. `127.0.0.1:12345` is the RECOMMENDED value documented
in `config.example.yaml`, not applied by the config loader.

#### Scenario: No authentication supported

- **WHEN** a client connects to the transparent listener
- **THEN** the listener SHALL NOT require any authentication
- **AND** SHALL process the connection as a redirected stream

#### Scenario: Custom listen address

- **WHEN** the configuration sets `transparent.listen: 127.0.0.1:12346`
- **THEN** the listener SHALL bind to `127.0.0.1:12346`
- **AND** the operator SHALL update the firewall REDIRECT target port to match

#### Scenario: No activating default port

- **WHEN** the `transparent` block is present but `transparent.listen` is
  omitted
- **THEN** the listener SHALL NOT bind any port
- **AND** SHALL be treated as disabled
