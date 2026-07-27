// Package socks5 is a minimal RFC 1928 inbound adapter that accepts CONNECT
// requests from local clients and tunnels them through the shared upstream
// cascade (internal/in/cascade).
//
// This implementation is deliberately narrow: it speaks only the "No
// Authentication Required" method (0x00) and only the CONNECT command
// (0x01). The BIND and UDP ASSOCIATE commands are refused with reply code
// 0x07. Username/Password authentication is not supported — the listener is
// localhost-only, matching the posture of the HTTP listener.
//
// UDP ASSOCIATE is intentionally unsupported: the configured upstream is an
// HTTPS-CONNECT (TCP-only) tunnel, so UDP packets have no transport through
// it. Advertising UDP support would mislead clients into attempting UDP
// rather than falling back to TCP relay.
//
// The package is split into two layers:
//
//   - Pure framing functions over io.Reader/io.Writer (NegotiateMethod,
//     ReadRequest, WriteReply). These are byte-level testable without any
//     net.Listener.
//   - A thin Serve shell that wires the framing into an Accept loop, the
//     upstream cascade, and bidirectional tunneling.
//
// No routing policy (PAC, RU-geo, whitelist, blacklist) is applied to SOCKS5
// connections — whatever destination the client requests is what gets
// tunneled. Routing decisions belong to the client (Telegram picks its own
// DCs); the listener is a dumb forwarder on top of the cascade.
package socks5
