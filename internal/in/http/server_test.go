package httpserver_test

import (
	"bufio"
	"errors"
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

// loadPAC returns a published pacPtr.
func loadPAC(t *testing.T, body string) *atomic.Pointer[[]byte] {
	t.Helper()
	p := &atomic.Pointer[[]byte]{}
	b := []byte(body)
	p.Store(&b)
	return p
}

// loadCfg returns a published configPtr with the given upstream.
func loadCfg(u port.Upstream) *atomic.Pointer[port.Config] {
	p := &atomic.Pointer[port.Config]{}
	c := port.Config{Listen: "127.0.0.1:0", Upstream: u}
	p.Store(&c)
	return p
}

// upstreamEcho is a minimal origin server that echoes the request line + body.
func upstreamEcho() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Saw", r.Host)
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("echo:"+r.URL.String()+":"), body...))
	}))
}

func TestServePAC_StatusAndContentType(t *testing.T) {
	t.Parallel()
	pac := loadPAC(t, "var PAC=1;\n")
	h := httpserver.New(nil, pac)

	req := httptest.NewRequest(http.MethodGet, httpserver.PACPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-ns-proxy-autoconfig" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Body.String() != "var PAC=1;\n" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestServePAC_DisablesCaching locks in the no-cache behavior. macOS does
// not re-fetch the PAC on a fixed timer — it honors HTTP cache headers, and
// with no Cache-Control the system heuristically caches for up to ~2 hours.
// For a hot-reloading PAC that's unacceptable: config edits would take that
// long to reach the browser. We force revalidation on every fetch.
func TestServePAC_DisablesCaching(t *testing.T) {
	t.Parallel()
	pac := loadPAC(t, "var PAC=1;\n")
	h := httpserver.New(nil, pac)

	req := httptest.NewRequest(http.MethodGet, httpserver.PACPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q (PAC must not be cached; hot-reload depends on this)", got, "no-cache")
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want %q (legacy HTTP/1.0 cache busting)", got, "no-cache")
	}
}

func TestServePAC_ReflectsSwappedSnapshot(t *testing.T) {
	t.Parallel()
	pac := loadPAC(t, "old")
	h := httpserver.New(nil, pac)

	// First GET sees "old".
	req := httptest.NewRequest(http.MethodGet, httpserver.PACPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "old" {
		t.Fatalf("first GET body = %q", rec.Body.String())
	}

	// Swap.
	newBody := []byte("new and longer")
	pac.Store(&newBody)

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Body.String() != "new and longer" {
		t.Errorf("post-swap body = %q, want %q", rec2.Body.String(), "new and longer")
	}
}

func TestForward_PlainHTTP_RelaysResponseAndBody(t *testing.T) {
	t.Parallel()
	origin := upstreamEcho()
	defer origin.Close()

	// No upstream configured → direct dial by transport. We point the proxy
	// at the origin directly by using a transport that ignores Proxy and
	// connects to the origin.
	// To exercise forwardPlain, we set upstream URL to empty so the transport
	// goes direct.
	cfg := loadCfg(port.Upstream{}) // empty URL → direct
	h := httpserver.New(cfg, loadPAC(t, ""))

	target := origin.URL + "/path?q=1"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Body = nil
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream-Saw"); got == "" {
		t.Errorf("X-Upstream-Saw header missing (origin didn't see the request)")
	}
	if !strings.Contains(rec.Body.String(), "echo:") {
		t.Errorf("body = %q, want echo prefix", rec.Body.String())
	}
}

func TestForward_AuthorizationHeaderAddedWhenCredsConfigured(t *testing.T) {
	t.Parallel()
	var sawAuth string
	var mu sync.Mutex

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()

	// Direct upstream (no proxy URL) but with creds — exercises setBasicAuth.
	cfg := loadCfg(port.Upstream{Username: "u", Password: "p"})
	h := httpserver.New(cfg, loadPAC(t, ""))

	req := httptest.NewRequest(http.MethodGet, origin.URL+"/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawAuth == "" {
		t.Error("origin did not receive Authorization header")
	}
}

func TestForward_CredentialsEmpty_NoAuthHeader(t *testing.T) {
	t.Parallel()
	var sawAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()

	cfg := loadCfg(port.Upstream{Username: "", Password: ""}) // both empty
	h := httpserver.New(cfg, loadPAC(t, ""))

	req := httptest.NewRequest(http.MethodGet, origin.URL+"/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if sawAuth != "" {
		t.Errorf("origin got Authorization = %q, want none", sawAuth)
	}
}

func TestForward_OnlyOneCredField_NoAuthHeader(t *testing.T) {
	t.Parallel()
	// Per spec: only when BOTH username AND password are non-empty.
	var sawAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()

	cfg := loadCfg(port.Upstream{Username: "u", Password: ""})
	h := httpserver.New(cfg, loadPAC(t, ""))

	req := httptest.NewRequest(http.MethodGet, origin.URL+"/x", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if sawAuth != "" {
		t.Errorf("origin got Authorization = %q, want none when password empty", sawAuth)
	}
}

func TestConnect_TunnelEstablishedAndDataCopied(t *testing.T) {
	t.Parallel()
	// A fake "origin" that speaks a known byte sequence once connected.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("hello-from-origin"))
	}()

	// Proxy with NO upstream URL → dialUpstream connects directly.
	cfg := loadCfg(port.Upstream{})
	h := httpserver.New(cfg, loadPAC(t, ""))

	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	// Issue a CONNECT to the proxy for the fake origin's address.
	proxyURL, _ := url.Parse(proxySrv.URL)
	conn, err := net.DialTimeout("tcp", proxyURL.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	target := ln.Addr().String()
	_, _ = conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"))

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// Read the bytes the origin wrote.
	buf := make([]byte, 64)
	n, err := br.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read from tunnel: %v", err)
	}
	if string(buf[:n]) != "hello-from-origin" {
		t.Errorf("tunneled data = %q, want hello-from-origin", buf[:n])
	}
}
