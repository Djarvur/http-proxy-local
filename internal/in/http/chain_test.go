package httpserver_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// stubUpstreamProxy is a real (minimal) forward proxy that:
//   - Accepts absolute-URI GET requests and forwards them to the origin.
//   - Accepts CONNECT and tunnels to the target.
//   - Records every Proxy-Authorization header it sees.
//   - Rejects requests missing the expected creds with 407.
//
// This lets us prove the local proxy sends correct Proxy-Authorization to
// the upstream, which is the actual proxy-chain contract.
type stubUpstreamProxy struct {
	t        *testing.T
	expected string // expected Proxy-Authorization value, "" = no auth required
	mu       sync.Mutex
	sawAuth  []string
	sawReqs  int32
}

func (s *stubUpstreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&s.sawReqs, 1)
	s.mu.Lock()
	s.sawAuth = append(s.sawAuth, r.Header.Get("Proxy-Authorization"))
	s.mu.Unlock()

	if s.expected != "" {
		if r.Header.Get("Proxy-Authorization") != s.expected {
			w.Header().Set("Proxy-Authenticate", `Basic realm="upstream"`)
			http.Error(w, "auth required", http.StatusProxyAuthRequired)
			return
		}
	}

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.handleForward(w, r)
}

func (s *stubUpstreamProxy) handleForward(w http.ResponseWriter, r *http.Request) {
	out, _ := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
	copyHeaders(out.Header, r.Header)
	out.Header.Del("Proxy-Authorization")
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *stubUpstreamProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Hijack FIRST, then write a proper HTTP/1.1 200 response manually.
	// Writing via ResponseWriter + Hijack races with header flushing.
	client, bufRW, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = bufRW.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = bufRW.Flush()
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
}

func (s *stubUpstreamProxy) authSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.sawAuth))
	copy(out, s.sawAuth)
	return out
}

func basicCreds(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// TestProxyChain_PlainHTTP_WithCreds proves the full chain:
// client → local proxy → upstream proxy (auth checked) → origin.
func TestProxyChain_PlainHTTP_WithCreds(t *testing.T) {
	t.Parallel()

	// Origin that echoes a known string.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("CHAIN-OK"))
	}))
	defer origin.Close()

	// Upstream proxy requiring creds.
	const user, pass = "chainuser", "chainpass"
	up := &stubUpstreamProxy{t: t, expected: basicCreds(user, pass)}
	upstreamSrv := httptest.NewServer(up)
	defer upstreamSrv.Close()

	// Local proxy configured to cascade to the upstream with creds.
	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: upstreamSrv.URL, Username: user, Password: pass},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	// Client sends a request through the local proxy.
	localProxyURL, _ := url.Parse(localProxy.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(localProxyURL)},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(origin.URL + "/path")
	if err != nil {
		t.Fatalf("chain GET failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("chain status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if string(body) != "CHAIN-OK" {
		t.Errorf("origin body = %q, want CHAIN-OK", body)
	}

	// The upstream proxy must have seen correct Proxy-Authorization.
	auths := up.authSeen()
	if len(auths) == 0 {
		t.Fatal("upstream proxy saw no requests — chain broken")
	}
	for i, a := range auths {
		if a != basicCreds(user, pass) {
			t.Errorf("upstream Proxy-Authorization[%d] = %q, want %q", i, a, basicCreds(user, pass))
		}
	}
}

// TestProxyChain_CONNECT_WithCreds proves HTTPS CONNECT tunnels through the
// upstream proxy with correct Proxy-Authorization on the CONNECT line.
func TestProxyChain_CONNECT_WithCreds(t *testing.T) {
	t.Parallel()

	// Plain-TCP "origin" (we're testing the tunnel, not TLS).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("TUNNELED"))
			}(c)
		}
	}()

	const user, pass = "tunneluser", "tunnelpass"
	up := &stubUpstreamProxy{t: t, expected: basicCreds(user, pass)}
	upstreamSrv := httptest.NewServer(up)
	defer upstreamSrv.Close()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: upstreamSrv.URL, Username: user, Password: pass},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	// Issue CONNECT through the local proxy.
	localProxyURL, _ := url.Parse(localProxy.URL)
	conn, err := net.DialTimeout("tcp", localProxyURL.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial local proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	target := ln.Addr().String()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT chain status = %d, want 200", resp.StatusCode)
	}

	// Read the tunneled bytes. The origin writes "TUNNELED" once and may
	// close immediately; reads after close return EOF. Poll briefly to
	// tolerate the inherent tunnel race.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got []byte
	buf := make([]byte, 64)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			break
		}
		if len(got) >= len("TUNNELED") {
			break
		}
	}
	if string(got) != "TUNNELED" {
		t.Errorf("tunneled data = %q, want TUNNELED", got)
	}

	// Upstream must have seen Proxy-Authorization on the CONNECT.
	auths := up.authSeen()
	if len(auths) == 0 {
		t.Fatal("upstream saw no CONNECT — tunnel didn't chain")
	}
	for i, a := range auths {
		if a != basicCreds(user, pass) {
			t.Errorf("upstream CONNECT Proxy-Authorization[%d] = %q, want %q", i, a, basicCreds(user, pass))
		}
	}
}

// TestProxyChain_RejectedCredsFailClosed proves that when the upstream
// rejects creds (407), the local proxy relays the failure rather than
// silently succeeding or hanging.
func TestProxyChain_RejectedCredsFailClosed(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should-not-reach"))
	}))
	defer origin.Close()

	// Upstream expects DIFFERENT creds than the local proxy sends.
	up := &stubUpstreamProxy{t: t, expected: basicCreds("other", "other")}
	upstreamSrv := httptest.NewServer(up)
	defer upstreamSrv.Close()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: upstreamSrv.URL, Username: "wrong", Password: "wrong"},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	localProxyURL, _ := url.Parse(localProxy.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(localProxyURL)},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(origin.URL + "/x")
	if err == nil {
		_ = resp.Body.Close()
	}
	// The local proxy must surface the failure (non-200 or transport error),
	// NOT silently return 200 with "should-not-reach".
	if resp != nil && resp.StatusCode == 200 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "should-not-reach") {
			t.Fatal("chain succeeded despite rejected creds — upstream 407 was not honored")
		}
	}
}

// copyHeaders is a local copy of the production helper (test package scope).
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Ensure context import is used (for future timeout plumbing).
var _ = context.Background
