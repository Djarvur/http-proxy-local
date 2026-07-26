// Package httpserver is the inbound HTTP adapter: it serves /proxy.pac and
// cascades all other requests to the configured upstream proxy. Per design
// D2, the live Config and PAC snapshot are read from atomic pointers, so
// every request sees a consistent snapshot without locks.
package httpserver

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/Djarvur/http-proxy-local/internal/in/cascade"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// PACPath is the single path at which the PAC is served.
const PACPath = "/proxy.pac"

// contentTypePAC is the standard PAC MIME type.
const contentTypePAC = "application/x-ns-proxy-autoconfig"

// New returns an http.Handler that serves /proxy.pac from pacPtr and forwards
// everything else to the configured upstream via configPtr.
//
// We deliberately do NOT use http.ServeMux: a CONNECT request has an
// authority-form URL (host:port, no path), and ServeMux's pattern matching
// dispatches by path. A custom handler dispatching on the URL path (for PAC)
// vs everything-else (forward) is the correct proxy semantics.
func New(configPtr *atomic.Pointer[port.Config], pacPtr *atomic.Pointer[[]byte]) http.Handler {
	pacHandler := servePAC(configPtr, pacPtr)
	forwardHandler := forward(configPtr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL != nil && r.URL.Path == PACPath && r.Method == http.MethodGet {
			pacHandler(w, r)
			return
		}
		forwardHandler(w, r)
	})
}

// servePAC returns the handler that serves the current PAC snapshot with
// cache headers derived from the live config's pac.cache_seconds.
func servePAC(configPtr *atomic.Pointer[port.Config], pacPtr *atomic.Pointer[[]byte]) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if pacPtr == nil {
			http.Error(w, "PAC not ready", http.StatusServiceUnavailable)
			return
		}
		pac := pacPtr.Load()
		if pac == nil {
			http.Error(w, "PAC not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", contentTypePAC)
		setPACCacheHeaders(w, configPtr)
		_, _ = w.Write(*pac)
	}
}

// setPACCacheHeaders emits the Cache-Control header per the live config:
//
//   - pac.cache_seconds omitted (nil) → default TTL (15s) → max-age=15, must-revalidate
//   - pac.cache_seconds = 0           → operator opt-out  → no-cache (+ Pragma: no-cache)
//   - pac.cache_seconds = N > 0       → max-age=N, must-revalidate
//
// must-revalidate forbids clients from serving a stale PAC if revalidation
// fails (e.g. proxy briefly unreachable) — better to drop the PAC than to
// route on stale rules. Reading per request means hot-reloaded
// pac.cache_seconds applies to the next request without restart.
//
// When configPtr is nil or holds no published config, we fall back to
// no-cache (the safest choice — never accidentally serve a long-cached PAC
// when we don't know the operator's intent).
func setPACCacheHeaders(w http.ResponseWriter, configPtr *atomic.Pointer[port.Config]) {
	seconds := 0 // safe default if config unavailable
	if configPtr != nil {
		if c := configPtr.Load(); c != nil {
			seconds = c.PAC.EffectiveCacheSeconds(port.DefaultPACCacheSeconds)
		}
	}
	if seconds <= 0 {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Pragma", "no-cache")
		return
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, must-revalidate", seconds))
}

// forward returns a handler that proxies plain-HTTP requests and CONNECT.
func forward(configPtr *atomic.Pointer[port.Config]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, configPtr)
			return
		}
		if !r.URL.IsAbs() {
			http.Error(w, "this proxy requires absolute-URI requests", http.StatusBadRequest)
			return
		}
		cfg := loadConfig(configPtr)
		if cfg == nil {
			http.Error(w, "proxy not configured", http.StatusServiceUnavailable)
			return
		}
		if err := forwardPlain(w, r, *cfg); err != nil {
			log.Printf("forward %s %s: %v", r.Method, r.URL.Host, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	}
}

// forwardPlain sends r to the configured upstream and relays the response.
// Per-request reading of cfg means a hot-reloaded upstream URL applies to the
// next request without restart (spec scenario: hot-reload of upstream).
func forwardPlain(w http.ResponseWriter, r *http.Request, cfg port.Config) error {
	transport := buildTransport(cfg.Upstream)
	out := r.Clone(r.Context())
	out.RequestURI = "" // server-side requests must clear this

	if cfg.Upstream.Username != "" && cfg.Upstream.Password != "" {
		setBasicAuth(out, cfg.Upstream.Username, cfg.Upstream.Password)
	}

	resp, err := transport.RoundTrip(out)
	if err != nil {
		return fmt.Errorf("upstream roundtrip: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Late-stage error: status already written; just log.
		log.Printf("copy response body for %s: %v", r.URL.Host, err)
	}
	return nil
}

// handleConnect tunnels a CONNECT request through the upstream proxy by:
//  1. Dialing the upstream (TCP or TLS based on its URL scheme).
//  2. Sending a CONNECT <target> request with optional Proxy-Authorization.
//  3. Reading the 200 response.
//  4. Bidirectionally copying bytes between client and upstream.
func handleConnect(w http.ResponseWriter, r *http.Request, configPtr *atomic.Pointer[port.Config]) {
	cfg := loadConfig(configPtr)
	if cfg == nil {
		http.Error(w, "proxy not configured", http.StatusServiceUnavailable)
		return
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

	upstreamConn, err := cascade.Dial(cfg.Upstream, target)
	if err != nil {
		log.Printf("CONNECT %s: upstream handshake: %v", target, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("CONNECT %s: hijack: %v", target, err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	cascade.Tunnel(clientConn, upstreamConn)
}

// buildTransport constructs an http.Transport that routes through the upstream
// proxy (with optional TLS for an https upstream URL).
func buildTransport(u port.Upstream) *http.Transport {
	t := &http.Transport{}
	if u.URL == "" {
		return t
	}
	proxyURL, err := url.Parse(u.URL)
	if err != nil {
		return t
	}
	if u.Username != "" && u.Password != "" {
		proxyURL.User = url.UserPassword(u.Username, u.Password)
	}
	t.Proxy = http.ProxyURL(proxyURL)
	return t
}

// setBasicAuth adds a Basic Authorization header for upstream credentials.
func setBasicAuth(r *http.Request, user, pass string) {
	r.SetBasicAuth(user, pass)
}

// copyHeaders copies src to dst verbatim.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// loadConfig returns the current Config, or nil if none is published yet.
func loadConfig(p *atomic.Pointer[port.Config]) *port.Config {
	if p == nil {
		return nil
	}
	return p.Load()
}
