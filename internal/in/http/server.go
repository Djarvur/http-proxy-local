// Package httpserver is the inbound HTTP adapter: it serves /proxy.pac and
// cascades all other requests to the configured upstream proxy. Per design
// D2, the live Config and PAC snapshot are read from atomic pointers, so
// every request sees a consistent snapshot without locks.
package httpserver

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// PACPath is the single path at which the PAC is served.
const PACPath = "/proxy.pac"

// contentTypePAC is the standard PAC MIME type.
const contentTypePAC = "application/x-ns-proxy-autoconfig"

// connectTimeout bounds the dial+CONNECT handshake to the upstream.
const connectTimeout = 30 * time.Second

// New returns an http.Handler that serves /proxy.pac from pacPtr and forwards
// everything else to the configured upstream via configPtr.
//
// We deliberately do NOT use http.ServeMux: a CONNECT request has an
// authority-form URL (host:port, no path), and ServeMux's pattern matching
// dispatches by path. A custom handler dispatching on the URL path (for PAC)
// vs everything-else (forward) is the correct proxy semantics.
func New(configPtr *atomic.Pointer[port.Config], pacPtr *atomic.Pointer[[]byte]) http.Handler {
	pacHandler := servePAC(pacPtr)
	forwardHandler := forward(configPtr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL != nil && r.URL.Path == PACPath && r.Method == http.MethodGet {
			pacHandler(w, r)
			return
		}
		forwardHandler(w, r)
	})
}

// servePAC returns the handler that serves the current PAC snapshot.
func servePAC(pacPtr *atomic.Pointer[[]byte]) http.HandlerFunc {
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
		_, _ = w.Write(*pac)
	}
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

	upstreamConn, err := dialUpstream(cfg.Upstream, target)
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
	tunnel(clientConn, upstreamConn)
}

// dialUpstream connects to the configured upstream proxy, sends a CONNECT
// for the target, and returns the established tunnel connection.
func dialUpstream(u port.Upstream, target string) (net.Conn, error) {
	if u.URL == "" {
		// No upstream configured — connect directly. (Useful for local testing.)
		return net.DialTimeout("tcp", target, connectTimeout)
	}

	parsed, err := url.Parse(u.URL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream url: %w", err)
	}

	conn, err := dialProxy(parsed)
	if err != nil {
		return nil, fmt.Errorf("dial upstream: %w", err)
	}

	// Send CONNECT.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.Username != "" && u.Password != "" {
		req += "Proxy-Authorization: Basic " + basicAuth(u.Username, u.Password) + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	// Read response status line + headers.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream CONNECT returned %d", resp.StatusCode)
	}

	// If bufio cached any bytes from the response body, they're already part
	// of the tunnel — wrap the conn so reads see them first.
	if br.Buffered() > 0 {
		return &bufferedConn{r: br, Conn: conn}, nil
	}
	return conn, nil
}

// dialProxy dials the proxy host with the appropriate transport (TLS for
// https upstream URLs, plain TCP otherwise).
func dialProxy(u *url.URL) (net.Conn, error) {
	host := u.Host
	if u.Scheme == "https" && !strings.Contains(host, ":") {
		host += ":443"
	} else if u.Scheme == "http" && !strings.Contains(host, ":") {
		host += ":80"
	}
	if u.Scheme == "https" {
		return tlsDial("tcp", host)
	}
	return net.DialTimeout("tcp", host, connectTimeout)
}

// tunnel copies bytes bidirectionally between two connections, returning when
// both directions complete. When one direction finishes (half-close), we
// half-close the write side of the peer so the peer's read loop can flush
// any remaining bytes; we do NOT set a deadline that would abort an
// in-flight read on the other side (a common bug that drops the last
// packet of a tunnel).
func tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		// Half-close: signal to b that a has no more to write. If b is a
		// *net.TCPConn this lets b's peer finish reading; CloseWrite is
		// a no-op for non-TCPConns (e.g. tls.Conn).
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
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

// basicAuth returns the base64-encoded "user:pass" string for the
// Proxy-Authorization header. (Equivalent to http.BasicAuth but standalone
// for the manual CONNECT path.)
func basicAuth(user, pass string) string {
	return base64StdEncoding(user + ":" + pass)
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

// bufferedConn wraps a net.Conn but serves buffered bytes first from a
// bufio.Reader. Used when CONNECT's response read left tunnel bytes in the
// reader's buffer.
type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
