package socks5

import (
	"context"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/cascade"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// SOCKS5 commands (CMD field in the request). Defined here (the consumer of
// ReadRequest's cmd byte) rather than in request.go — the parser returns the
// raw byte; the policy of which commands to accept lives here.
const (
	cmdConnect      byte = 0x01
	cmdBind         byte = 0x02
	cmdUDPAssociate byte = 0x03
)

// Reply codes (REP field in the reply).
const (
	repSuccess         byte = 0x00
	repFailure         byte = 0x01
	repCmdNotSupported byte = 0x07
)

// handshakeTimeout bounds the SOCKS5 greeting + request phase. After the
// tunnel is established the deadline is cleared (Telegram holds sockets open
// for long periods; an idle timeout would sever live push connections).
const handshakeTimeout = 10 * time.Second

// Serve runs the SOCKS5 Accept loop on ln until ctx is cancelled. Each
// accepted connection is handled in its own goroutine: method negotiation,
// request parsing, command dispatch (CONNECT only; BIND/UDP refused), and
// tunneling through the shared upstream cascade.
//
// cfgPtr is read per-connection so a hot-reloaded upstream.URL applies to
// the next accepted connection without restart (design D4). The listen
// address itself is bind-time (restart required) — managed by the caller.
//
// Serve returns when ctx is cancelled. The caller is responsible for
// closing ln; Serve does not own it (the composition root may want to log
// the bound address etc. before/after).
func Serve(ctx context.Context, ln net.Listener, cfgPtr *atomic.Pointer[port.Config]) {
	// Background goroutine to close the listener on ctx cancellation. This
	// unblocks the Accept loop (a closed listener returns an error from
	// Accept, which we treat as the shutdown signal).
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Accept errors after ctx cancellation are the normal shutdown
			// path; anything else is logged and treated as fatal-ish (the
			// loop exits, surfacing the failure to the caller).
			if ctx.Err() != nil {
				return
			}
			log.Printf("socks5 accept: %v", err)
			return
		}
		go handle(conn, cfgPtr)
	}
}

// handle processes a single SOCKS5 connection end-to-end. It owns closing
// the client conn.
//
// handle does not take a context: cancellation is signaled by the caller
// closing the listener (which terminates Serve's Accept loop), not by
// per-connection cancellation. The cascade's CONNECT handshake is bounded
// by its own internal timeout; long-lived tunnels have no deadline by
// design (Telegram holds sockets open for push).
func handle(conn net.Conn, cfgPtr *atomic.Pointer[port.Config]) {
	defer func() { _ = conn.Close() }()

	// Bound the handshake phase against slowloris-style hangs.
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))

	if err := NegotiateMethod(conn, conn); err != nil {
		// Negotiation already wrote the appropriate reply (0xFF) or none
		// (malformed). Just close. Don't log the common "client offered no
		// acceptable method" case loudly — it's normal for non-SOCKS5
		// scanners to hit the port.
		return
	}

	cmd, target, _, err := ReadRequest(conn)
	if err != nil {
		return
	}

	// Dispatch on command. Only CONNECT is supported; BIND and UDP
	// ASSOCIATE are refused with 0x07 per design D5.
	if cmd != cmdConnect {
		// Clear the handshake deadline before writing the reply so a slow
		// write doesn't race with the deadline.
		_ = conn.SetReadDeadline(time.Time{})
		_ = WriteReply(conn, repCmdNotSupported)
		return
	}

	// Read the live config for THIS connection (D4: hot-reloadable upstream).
	// No config published → general failure (mirrors the HTTP listener's 503).
	cfg := loadConfig(cfgPtr)
	if cfg == nil {
		_ = conn.SetReadDeadline(time.Time{})
		_ = WriteReply(conn, repFailure)
		log.Printf("socks5 CONNECT %s: no config published", target)
		return
	}

	// The cascade call is bounded by its own connectTimeout (30s); we don't
	// thread ctx into Dial because Dial is shared with the HTTP listener
	// (which has no per-connection ctx). Cancellation is handled by the
	// caller closing the listener, which terminates new Accepts.
	upstreamConn, err := cascade.Dial(cfg.Upstream, target)
	if err != nil {
		_ = conn.SetReadDeadline(time.Time{})
		_ = WriteReply(conn, repFailure)
		log.Printf("socks5 CONNECT %s: cascade: %v", target, err)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	// Success — clear the handshake deadline and tunnel. The tunnel phase
	// has NO deadline (Telegram holds sockets open indefinitely for push).
	_ = conn.SetReadDeadline(time.Time{})
	if err := WriteReply(conn, repSuccess); err != nil {
		return
	}
	cascade.Tunnel(conn, upstreamConn)
}

// loadConfig returns the live Config from cfgPtr, or nil if no config is
// published yet. Returning the pointer (not a zero-value copy) lets callers
// distinguish "no config" from "config with an empty upstream URL" — the
// latter is a valid direct-mode configuration.
func loadConfig(cfgPtr *atomic.Pointer[port.Config]) *port.Config {
	if cfgPtr == nil {
		return nil
	}
	return cfgPtr.Load()
}
