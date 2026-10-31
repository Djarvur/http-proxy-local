// Package transparent is the inbound transparent (redirect-capture) adapter:
// it accepts TCP connections the local firewall redirected to it (iptables
// REDIRECT), recovers the pre-redirect destination via SO_ORIGINAL_DST, routes
// the destination through the shared dialchain (same precedence and dial
// behavior as the smart HTTP listener), and relays the raw TCP stream without
// any protocol overhead. It replaces the external redsocks daemon in the
// interception chain (change add-transparent-listener).
//
// The listener carries no client authentication — it inherits the
// localhost-only posture of the other in-adapters; it only ever sees
// connections the local firewall decided to redirect.
package transparent

import (
	"context"
	"log"
	"net"
	"sync/atomic"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/in/cascade"
	"github.com/Djarvur/http-proxy-local/internal/in/dialchain"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// Server owns the transparent accept loop. It bundles the dialchain seams
// (original-destination source, resolver, direct dialer, upstream cascade)
// and reads config/ranges per connection, so hot-reloaded routing rules apply
// to the next accepted connection.
type Server struct {
	cfgPtr    *atomic.Pointer[port.Config]
	rangesPtr *atomic.Pointer[port.RURanges]
	// origDst recovers the pre-redirect destination as host:port. Real usage
	// is the SO_ORIGINAL_DST getsockopt (originaldst_linux.go); tests inject
	// fakes.
	origDst func(context.Context, net.Conn) (string, error)
	resolve dialchain.ResolveFunc
	dialer  dialchain.DirectDialFunc
	// upstream dials the configured cascade; nil means cascade.Dial with the
	// live cfg.Upstream. Tests inject fakes.
	upstream dialchain.UpstreamDialFunc
	// negCache is shared across connections handled by this server — the
	// negative cache is host-level state, per spec.
	negCache *dialchain.NegativeCache
	// logf is the log sink for per-connection lines (warnings, failures).
	// nil means the standard logger; tests inject a buffer instead of swapping
	// the process-global log writer (which races with parallel tests).
	logf func(format string, args ...any)
}

// NewServer constructs a transparent listener server wired to the real
// adapters: SO_ORIGINAL_DST destination recovery, the system resolver, a raw
// net.Dialer for Direct strategies, and cascade.Dial for Upstream. Tests
// construct Server literals directly to inject fakes.
func NewServer(
	cfgPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	resolver *net.Resolver,
) *Server {
	resolve := func(ctx context.Context, host string) ([]net.IP, error) {
		// LookupIPAddr returns the system resolver's view (honouring
		// /etc/hosts, its own cache, negative answers). No second cache.
		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		return ips, nil
	}
	return &Server{
		cfgPtr:    cfgPtr,
		rangesPtr: rangesPtr,
		origDst:   OriginalDst,
		resolve:   resolve,
		dialer:    (&net.Dialer{}).DialContext,
		negCache:  dialchain.NewNegativeCache(),
	}
}

// Serve runs the accept loop on ln until ctx is cancelled, mirroring the
// SOCKS5 listener's lifecycle: a goroutine closes ln on ctx.Done, Accept
// errors after cancellation are the normal shutdown path, anything before is
// logged and treated as fatal-ish (the loop exits, surfacing to the caller).
// Each accepted connection is handled in its own goroutine.
func (s *Server) Serve(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logfOf()("transparent accept: %v", err)
			return
		}
		go s.HandleConn(ctx, conn)
	}
}

// HandleConn processes a single redirected connection end-to-end: recover
// the original destination, warn when routing resolves to a Direct-capable
// strategy list, dial via the shared chain, and relay raw bytes until either
// side closes. It owns closing the client conn.
func (s *Server) HandleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	target, err := s.origDst(ctx, conn)
	if err != nil {
		s.logfOf()("transparent: recover original destination: %v", err)
		return
	}

	cfg := s.cfgPtr.Load()
	if cfg == nil {
		s.logfOf()("transparent %s: no config published", target)
		return
	}
	rg := port.RURanges{}
	if r := s.rangesPtr.Load(); r != nil {
		rg = *r
	}

	// Loop-risk warning (design D4): when the strategy list contains Direct,
	// the dial goes straight at the destination — a loop if that destination
	// sits in the interception ipset. The warning surfaces the deployment
	// convention (never blacklist interception-ipset CIDRs); it does not
	// block or alter routing.
	if hasDirectStrategy(s.resolve, target, *cfg, rg) {
		s.logfOf()("transparent %s: WARNING routing resolves to Direct — this destination must NOT be in the interception ipset (loop risk)", target)
	}

	upstreamFn := s.upstream
	if upstreamFn == nil {
		upstreamFn = func(t string) (net.Conn, error) { return cascade.Dial(cfg.Upstream, t) }
	}
	chain := dialchain.New(s.resolve, s.dialer, upstreamFn, s.negCache)
	outbound, err := chain.Dial(ctx, target, *cfg, rg)
	if err != nil {
		s.logfOf()("transparent %s: all strategies failed: %v", target, err)
		return
	}
	defer func() { _ = outbound.Close() }()

	// Raw relay — no protocol, no deadlines (long-lived MTProto tunnels are
	// the primary client of this listener).
	cascade.Tunnel(conn, outbound)
}

// logfOf returns the effective log sink: the injected one when set, otherwise
// the standard logger.
func (s *Server) logfOf() func(format string, args ...any) {
	if s.logf != nil {
		return s.logf
	}
	return log.Printf
}

// hasDirectStrategy reports whether app.Decide for the target produces a
// Direct strategy, given the same resolution the chain's Dial will perform.
func hasDirectStrategy(resolver dialchain.ResolveFunc, target string, cfg port.Config, ranges port.RURanges) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	ips, err := resolver(context.Background(), host)
	if err != nil {
		ips = nil
	}
	for _, s := range app.Decide(host, ips, cfg, ranges) {
		if s.Path == app.StrategyDirect {
			return true
		}
	}
	return false
}
