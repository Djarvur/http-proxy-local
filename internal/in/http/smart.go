package httpserver

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/in/cascade"
	"github.com/Djarvur/http-proxy-local/internal/in/dialchain"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// resolveFunc resolves a hostname to its IPs. The handler injects an adaptor
// over net.Resolver.LookupIPAddr; tests inject fakes.
type resolveFunc = dialchain.ResolveFunc

// directDialFunc dials a Direct target (by hostname or by a pinned IP). It is
// the seam through which the Direct path is tested: real usage hands in
// net.Dialer.DialContext; tests hand in spies.
type directDialFunc = dialchain.DirectDialFunc

// upstreamDialFunc dials the configured upstream cascade to a target
// host:port. Real usage hands in a closure that captures the live
// port.Upstream and calls cascade.Dial; tests hand in spies.
type upstreamDialFunc = dialchain.UpstreamDialFunc

// NewSmart returns an http.Handler that applies the PAC-equivalent routing
// precedence server-side for each request and dials the target via the
// ordered strategy list produced by app.Decide. It does NOT serve /proxy.pac
// (returns 404) — PAC-blind clients cannot use a PAC anyway, and the PAC's
// PROXY directive would point at the dumb listener.
//
// The handler reads configPtr and rangesPtr per request, so hot-reloaded
// routing rules apply to the next request with no restart. The three smart
// tuning fields (dial_timeout, dial_attempts, negative_cache_ttl) are likewise
// per-request reads.
func NewSmart(
	configPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	resolver *net.Resolver,
	negCache *negativeCache,
) http.Handler {
	resolve := func(ctx context.Context, host string) ([]net.IP, error) {
		// LookupIPAddr returns the system resolver's view (honouring /etc/hosts,
		// its own cache, negative answers). We do not add a second cache.
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
	return newSmartHandler(configPtr, rangesPtr, resolve, (&net.Dialer{}).DialContext, negCache)
}

// newSmartHandler is the testable core of NewSmart: it accepts the resolve
// and direct-dial seams so end-to-end tests can drive routing decisions
// without depending on the system resolver or real network dialing. The
// upstream path always uses cascade.Dial with the live cfg.Upstream.
func newSmartHandler(
	configPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	resolve resolveFunc,
	direct directDialFunc,
	negCache *negativeCache,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PAC is not served from the smart listener (design D7).
		if r.Method == http.MethodConnect {
			smartHandleConnect(w, r, configPtr, rangesPtr, resolve, direct, negCache)
			return
		}
		if r.URL != nil && r.URL.Path == PACPath {
			http.NotFound(w, r)
			return
		}
		smartForward(w, r, configPtr, rangesPtr, resolve)
	})
}

// smartDial resolves the target host (only if a later rule needs an IP),
// evaluates the precedence via app.Decide, and walks the resulting strategy
// list in order until one establishes a connection.
//
// The behavior (Direct timeout/attempt bounds, negative-cache consult/record,
// RU-IP pinning, upstream fallback) lives in the shared dialchain package
// (design D1) so the transparent listener walks strategies identically.
// Tuning is read from cfg.Smart in both listeners — one tuning block governs
// the two strategy-dialing listeners.
func smartDial(
	ctx context.Context,
	target string, // host:port
	cfg port.Config,
	ranges port.RURanges,
	resolve resolveFunc,
	direct directDialFunc,
	upstream upstreamDialFunc,
	negCache *negativeCache,
) (net.Conn, error) {
	chain := dialchain.New(resolve, direct, upstream, negCache)
	return chain.Dial(ctx, target, cfg, ranges)
}

// smartHandleConnect hijacks the client connection, dials the target via
// smartDial, and on success replies 200 and tunnels bidirectionally. On
// failure it closes the client connection. Mirrors handleConnect in server.go
// but routes through the strategy walker.
//
// Note: the handler-level upstream closure is not passed here — CONNECT
// builds its own upstreamFn capturing the live cfg.Upstream so the cascade
// sees the per-request config snapshot.
func smartHandleConnect(
	w http.ResponseWriter,
	r *http.Request,
	configPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	resolve resolveFunc,
	direct directDialFunc,
	negCache *negativeCache,
) {
	cfg := configPtr.Load()
	if cfg == nil {
		http.Error(w, "proxy not configured", http.StatusServiceUnavailable)
		return
	}
	ranges := rangesPtr.Load()
	if ranges == nil {
		empty := port.RURanges{}
		ranges = &empty
	}

	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	if target == "" {
		http.Error(w, "CONNECT requires a host", http.StatusBadRequest)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	// Wrap the upstream closure to carry the live cfg.Upstream.
	upstreamFn := func(t string) (net.Conn, error) { return cascade.Dial(cfg.Upstream, t) }

	conn, err := smartDial(r.Context(), target, *cfg, *ranges, resolve, direct, upstreamFn, negCache)
	if err != nil {
		log.Printf("smart CONNECT %s: %v", target, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = conn.Close() }()

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("smart CONNECT %s: hijack: %v", target, err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	cascade.Tunnel(clientConn, conn)
}

// smartForward forwards a plain-HTTP (non-CONNECT) request. It evaluates the
// precedence, selects the first successful strategy, builds a transport, and
// relays the response. Plain HTTP does NOT pin the RU IP (the transport dials
// by hostname) — per design, plain HTTP is a negligible fraction of traffic
// and the custom DialContext complexity is not justified.
func smartForward(
	w http.ResponseWriter,
	r *http.Request,
	configPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	resolve resolveFunc,
) {
	if !r.URL.IsAbs() {
		http.Error(w, "this proxy requires absolute-URI requests", http.StatusBadRequest)
		return
	}
	cfg := configPtr.Load()
	if cfg == nil {
		http.Error(w, "proxy not configured", http.StatusServiceUnavailable)
		return
	}
	ranges := rangesPtr.Load()
	if ranges == nil {
		empty := port.RURanges{}
		ranges = &empty
	}

	host := r.URL.Hostname()
	ips, _ := resolve(r.Context(), host)
	strategies := app.Decide(host, ips, *cfg, *ranges)
	if len(strategies) == 0 {
		http.Error(w, "no route", http.StatusBadGateway)
		return
	}

	// Pick the first strategy; plain HTTP does not retry across strategies.
	// For Direct, build a no-proxy transport; for Upstream, build the cascade
	// transport (same as the dumb listener). Both dial by hostname.
	var transport http.RoundTripper
	switch strategies[0].Path {
	case app.StrategyDirect:
		transport = &http.Transport{} // direct dial by hostname
	case app.StrategyUpstream:
		transport = buildTransport(cfg.Upstream)
	default:
		transport = buildTransport(cfg.Upstream)
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	if cfg.Upstream.Username != "" && cfg.Upstream.Password != "" && strategies[0].Path == app.StrategyUpstream {
		setBasicAuth(out, cfg.Upstream.Username, cfg.Upstream.Password)
	}

	resp, err := transport.RoundTrip(out)
	if err != nil {
		log.Printf("smart forward %s %s: %v", r.Method, r.URL.Host, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("smart copy response body for %s: %v", r.URL.Host, err)
	}
}
