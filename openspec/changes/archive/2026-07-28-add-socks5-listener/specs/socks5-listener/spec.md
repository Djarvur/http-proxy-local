# socks5-listener Specification

## Purpose

A SOCKS5 (RFC 1928) inbound listener that accepts CONNECT requests from local
clients (notably the Telegram client, which cannot use an HTTP/HTTPS proxy) and
tunnels each accepted connection through the same HTTPS-CONNECT upstream
cascade used by the HTTP listener. This capability covers the SOCKS5 wire
protocol, the deliberate support matrix (CONNECT-only, no auth, no BIND, no
UDP ASSOCIATE), the opt-in lifecycle (listener only starts when the `socks5`
config block is present), and the deliberate absence of any routing layer
(PAC, RU-geo, whitelist, blacklist are NOT applied to SOCKS5 connections).

The capability does NOT cover: SOCKS5 username/password authentication,
the BIND command, UDP ASSOCIATE, IPv6 listen-address parity with future
geo work, or per-listener upstream overrides. These are explicitly out of
scope for this change.

## Requirements

### Requirement: Speak the SOCKS5 greeting and method negotiation

The listener SHALL accept inbound TCP connections on the configured
`socks5.listen` address and perform RFC 1928 method negotiation. The listener
SHALL offer and accept only the "No Authentication Required" method (`0x00`).
The listener SHALL reject any greeting that does not offer method `0x00` by
responding with method `0xFF` (No Acceptable Methods) and closing the
connection. The listener SHALL reject any greeting with a SOCKS version byte
other than `0x05` by closing the connection without a method-selection reply.

#### Scenario: No-auth method accepted
- **WHEN** a client sends a valid SOCKS5 greeting offering method `0x00` (No Authentication Required)
- **THEN** the listener SHALL reply with method byte `0x00`
- **AND** SHALL proceed to the request phase

#### Scenario: No acceptable methods
- **WHEN** a client sends a valid SOCKS5 greeting that does NOT offer method `0x00` (e.g. offers only `0x02` Username/Password)
- **THEN** the listener SHALL reply with method byte `0xFF` (No Acceptable Methods)
- **AND** SHALL close the connection

#### Scenario: Wrong SOCKS version rejected
- **WHEN** a client sends a greeting whose first byte is not `0x05`
- **THEN** the listener SHALL close the connection without sending a method-selection reply

#### Scenario: Malformed greeting rejected
- **WHEN** a client sends a greeting that is too short to contain a complete method list (fewer bytes than declared NMETHODS, or fewer than 2 bytes total)
- **THEN** the listener SHALL close the connection without sending a reply

### Requirement: Support only the CONNECT command

The listener SHALL accept the CONNECT command (`0x01`) and SHALL refuse the
BIND (`0x02`) and UDP ASSOCIATE (`0x03`) commands by replying with code `0x07`
(Command not supported) and closing the connection. UDP ASSOCIATE is
deliberately unsupported because the configured upstream is an HTTPS-CONNECT
(TCP-only) tunnel — UDP packets have no transport through it, and advertising
UDP support would mislead clients into attempting UDP rather than falling back
to TCP relay.

#### Scenario: CONNECT command accepted
- **WHEN** the client sends a SOCKS5 request with command byte `0x01` (CONNECT)
- **THEN** the listener SHALL proceed to establish the upstream tunnel

#### Scenario: BIND command refused
- **WHEN** the client sends a SOCKS5 request with command byte `0x02` (BIND)
- **THEN** the listener SHALL reply with REP code `0x07` (Command not supported)
- **AND** SHALL close the connection

#### Scenario: UDP ASSOCIATE command refused
- **WHEN** the client sends a SOCKS5 request with command byte `0x03` (UDP ASSOCIATE)
- **THEN** the listener SHALL reply with REP code `0x07` (Command not supported)
- **AND** SHALL close the connection

#### Scenario: Unknown command refused
- **WHEN** the client sends a SOCKS5 request with a command byte other than `0x01`, `0x02`, or `0x03`
- **THEN** the listener SHALL reply with REP code `0x07` (Command not supported)
- **AND** SHALL close the connection

### Requirement: Parse all three SOCKS5 address types for CONNECT

For an accepted CONNECT request, the listener SHALL parse the destination
address regardless of address type: IPv4 (`0x01`), domain name (`0x03`), and
IPv6 (`0x04`). The parsed destination SHALL be formatted as a `host:port`
string (with the host bracketed for IPv6 per RFC 3986) and handed to the
upstream-cascade dialer. The listener SHALL NOT perform DNS resolution
itself — the configured upstream resolves the name via its own CONNECT
handshake.

#### Scenario: IPv4 destination tunneled
- **WHEN** the client sends a CONNECT request with ATYP `0x01` and a 4-byte IPv4 address
- **THEN** the listener SHALL format the destination as `a.b.c.d:port`
- **AND** SHALL hand it to the upstream cascade

#### Scenario: Domain destination tunneled
- **WHEN** the client sends a CONNECT request with ATYP `0x03` and a length-prefixed domain name
- **THEN** the listener SHALL format the destination as `domain:port`
- **AND** SHALL hand it to the upstream cascade without performing local DNS resolution

#### Scenario: IPv6 destination tunneled
- **WHEN** the client sends a CONNECT request with ATYP `0x04` and a 16-byte IPv6 address
- **THEN** the listener SHALL format the destination as `[ipv6]:port`
- **AND** SHALL hand it to the upstream cascade

#### Scenario: Malformed request rejected
- **WHEN** the client sends a CONNECT request that is too short for its declared ATYP (e.g. ATYP `0x01` but fewer than 4 trailing address bytes)
- **THEN** the listener SHALL close the connection without establishing a tunnel

### Requirement: Tunnel accepted CONNECT requests through the shared upstream cascade

For each accepted CONNECT, the listener SHALL establish a tunneled TCP
connection to the destination by invoking the same upstream-cascade code used
by the HTTP listener (dial the configured `upstream` over HTTP or HTTPS, send
an HTTP CONNECT to the destination, read the `200` response). On success the
listener SHALL reply with REP code `0x00` (succeeded) and SHALL then tunnel
bytes bidirectionally between the client and the upstream connection until
either side closes. The listener SHALL apply NO routing policy (PAC, RU-geo,
whitelist, blacklist) to SOCKS5 connections — whatever destination the client
requested is what gets tunneled.

#### Scenario: CONNECT succeeds and tunnels
- **WHEN** the upstream cascade establishes a tunnel to the requested destination
- **THEN** the listener SHALL reply with REP code `0x00` and BND.ADDR/BND.PORT set to `0.0.0.0:0`
- **AND** SHALL tunnel bytes bidirectionally until either side closes

#### Scenario: Upstream cascade failure
- **WHEN** the upstream cascade fails to establish a tunnel (upstream unreachable, CONNECT rejected with non-200, config not published)
- **THEN** the listener SHALL reply with REP code `0x01` (general SOCKS server failure)
- **AND** SHALL close the client connection
- **AND** SHALL log the failure (errors-only, no per-connection success logging)

#### Scenario: No routing policy applied
- **WHEN** the client requests a destination that would match the blacklist, whitelist, local-network, or RU-geo rules in the live configuration
- **THEN** the listener SHALL tunnel the request to the requested destination without consulting any routing policy
- **AND** SHALL NOT apply DIRECT-vs-PROXY decision logic

### Requirement: Apply a handshake read deadline and no tunnel deadline

The listener SHALL apply a read deadline covering the SOCKS5 greeting and
request phases (method negotiation through the complete CONNECT request).
The deadline SHALL bound the listener against slowloris-style hangs during
handshake. Once the CONNECT request has been accepted and the tunnel phase
begins, the listener SHALL clear read deadlines — the tunnel MAY stay open
indefinitely for long-lived connections (e.g. Telegram sockets held open for
push delivery).

#### Scenario: Slow handshake dropped
- **WHEN** a client opens a connection and sends no greeting bytes within the handshake deadline
- **THEN** the listener SHALL close the connection

#### Scenario: Long-lived tunnel preserved
- **WHEN** a CONNECT tunnel is established and the client and upstream exchange no bytes for an extended period (minutes)
- **THEN** the listener SHALL keep the tunnel open
- **AND** SHALL NOT impose an idle timeout that would sever the connection

### Requirement: Start only when the socks5 config block is enabled

The SOCKS5 listener SHALL start if and only if `socks5.listen` is non-empty
at process startup. When `socks5.listen` is empty (whether because the
`socks5` block is absent or because the field is explicitly set to empty), the
listener SHALL NOT bind a port and the process SHALL behave identically to a
build without this feature (full backward compatibility). The listener SHALL
bind to the configured address at startup; subsequent changes to
`socks5.listen` via hot-reload SHALL require a process restart and SHALL log
an error explaining this.

#### Scenario: Listener starts when configured
- **WHEN** the configuration sets `socks5.listen: 127.0.0.1:1080`
- **THEN** the listener SHALL bind to `127.0.0.1:1080` at process startup
- **AND** SHALL accept SOCKS5 connections

#### Scenario: Listener does not start when block absent
- **WHEN** the configuration omits the `socks5` block entirely
- **THEN** the listener SHALL NOT bind any port
- **AND** the process SHALL behave identically to a build without this feature

#### Scenario: Listener does not start when listen is empty
- **WHEN** the configuration sets `socks5.listen: ""` (explicit opt-out)
- **THEN** the listener SHALL NOT bind any port

#### Scenario: listen change requires restart
- **WHEN** the `socks5.listen` field is edited in config while the server is running
- **THEN** the system SHALL log an error stating that a restart is required to apply the new socks5 listen address
- **AND** SHALL NOT rebind the listener mid-flight

#### Scenario: socks5 block added or removed requires restart
- **WHEN** the `socks5` block is added to (or removed from) config while the server is running
- **THEN** the system SHALL log an error stating that a restart is required
- **AND** SHALL NOT start or stop the listener mid-flight

### Requirement: Carry no client authentication and bind only the configured address

The listener SHALL NOT support client authentication — only the No
Authentication Required method (`0x00`) SHALL be accepted, matching the
localhost-only, no-client-auth posture of the existing HTTP listener. The
listener SHALL bind only to the address the operator explicitly wrote in
`socks5.listen`; there is NO activating default port. The listener SHALL NOT
bind to wildcard interfaces unless the operator explicitly configures one.
The value `127.0.0.1:1080` is the RECOMMENDED value documented in
`config.example.yaml`, but it is not applied by the config loader — the
operator MUST write it (or any other non-empty value) to enable the listener.

#### Scenario: No authentication supported
- **WHEN** a client attempts to negotiate Username/Password authentication (method `0x02`)
- **THEN** the listener SHALL reject the method per the "no acceptable methods" rule

#### Scenario: Custom listen address
- **WHEN** the configuration sets `socks5.listen: 127.0.0.1:1085`
- **THEN** the listener SHALL bind to `127.0.0.1:1085`

#### Scenario: No activating default port
- **WHEN** the `socks5` block is present but `socks5.listen` is omitted
- **THEN** the listener SHALL NOT bind any port
- **AND** SHALL be treated as disabled (operator must write a non-empty `socks5.listen` to enable)
