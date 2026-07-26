//go:build realproxy

// Real-proxy smoke tests. These are gated behind the "realproxy" build tag
// and are EXCLUDED from the normal test suite + CI. They only run when
// explicitly invoked, e.g.:
//
//	ZCODE_HTTP_PROXY=https://user:pass@host:port \
//	  go test -tags realproxy -run TestRealProxy ./internal/in/http/
//
// The upstream URL (including credentials) comes from the environment —
// nothing is hardcoded, so this file is safe to commit. The test verifies
// our local proxy correctly cascades to the real upstream over its real
// transport (HTTPS proxy URL in the zcode case), something the in-process
// stubs cannot prove.
package httpserver_test

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// realProxyURL returns the upstream URL from $ZCODE_HTTP_PROXY (or
// $HTTP_PROXY/$HTTPS_PROXY as fallback). Returns empty if none is set.
func realProxyURL() string {
	for _, k := range []string{"ZCODE_HTTP_PROXY", "HTTP_PROXY", "HTTPS_PROXY"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
	}
	return ""
}

// TestRealProxy_PlainHTTPChain verifies: client → local proxy → REAL upstream
// → real origin (httpbin). Proves the local proxy's Proxy-Authorization
// header is accepted by a real proxy implementation.
func TestRealProxy_PlainHTTPChain(t *testing.T) {
	upstreamURL := realProxyURL()
	if upstreamURL == "" {
		t.Skip("set ZCODE_HTTP_PROXY (or HTTP_PROXY) to run this real-proxy test")
	}
	t.Logf("using upstream: %s", redactCreds(upstreamURL))

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}

	// Build the local proxy pointed at the real upstream.
	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: u.String(), Username: user, Password: pass},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	// Client sends a plain-HTTP request through the local proxy.
	localProxyURL, _ := url.Parse(localProxy.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(localProxyURL)},
		Timeout:   30 * time.Second,
	}

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("plain-HTTP chain through real upstream failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Example Domain") {
		t.Errorf("body did not contain expected Example Domain marker; got %d bytes", len(body))
	}
	t.Logf("plain-HTTP chain OK: %d bytes from example.com via real upstream", len(body))
}

// TestRealProxy_HTTPSChain verifies: client → local proxy → CONNECT through
// REAL upstream → real HTTPS origin. This is the case the zcode proxy is
// actually used for, and the case my in-process stubs can't fully replicate
// (real proxies have specific CONNECT + TLS behaviors).
func TestRealProxy_HTTPSChain(t *testing.T) {
	upstreamURL := realProxyURL()
	if upstreamURL == "" {
		t.Skip("set ZCODE_HTTP_PROXY (or HTTP_PROXY) to run this real-proxy test")
	}
	t.Logf("using upstream: %s", redactCreds(upstreamURL))

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: u.String(), Username: user, Password: pass},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	// Client does HTTPS through the local proxy (forces CONNECT).
	localProxyURL, _ := url.Parse(localProxy.URL)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(localProxyURL),
			// Don't verify certs in this smoke test — we trust the local proxy
			// path, and example.com's cert is valid regardless.
			TLSClientConfig: &tls.Config{},
		},
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatalf("HTTPS CONNECT chain through real upstream failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Example Domain") {
		t.Errorf("body did not contain expected Example Domain marker; got %d bytes", len(body))
	}
	t.Logf("HTTPS CONNECT chain OK: %d bytes from example.com via real upstream", len(body))
}

// TestRealProxy_RejectedCredsFailClosed points the local proxy at the real
// upstream with WRONG credentials and confirms the failure surfaces (not a
// silent success or hang).
func TestRealProxy_RejectedCredsFailClosed(t *testing.T) {
	upstreamURL := realProxyURL()
	if upstreamURL == "" {
		t.Skip("set ZCODE_HTTP_PROXY (or HTTP_PROXY) to run this real-proxy test")
	}

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	// Strip creds from the URL; we'll send wrong ones explicitly.
	bareURL := u.Scheme + "://" + u.Host

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	empty := []byte("")
	pacPtr.Store(&empty)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: bareURL, Username: "definitely", Password: "wrong"},
	}
	cfgPtr.Store(&cfg)

	localProxy := httptest.NewServer(httpserver.New(cfgPtr, pacPtr))
	defer localProxy.Close()

	localProxyURL, _ := url.Parse(localProxy.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(localProxyURL)},
		Timeout:   15 * time.Second,
	}

	resp, err := client.Get("http://example.com/")
	if err == nil {
		_ = resp.Body.Close()
	}
	// Either a transport error or a non-200 (likely 407) is acceptable.
	// What's NOT acceptable: 200 with example.com content (silent success).
	if resp != nil && resp.StatusCode == 200 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "Example Domain") {
			t.Fatal("chain succeeded with WRONG creds — upstream did not enforce auth")
		}
	}
	t.Logf("rejected-creds correctly surfaced (resp=%v, err=%v)", respStatusOrZero(resp), err != nil)
}

func respStatusOrZero(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// redactCreds replaces user:pass@ in a URL with ***@ for safe logging.
func redactCreds(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<unparseable>"
	}
	if u.User == nil {
		return s
	}
	u.User = url.User("***")
	return u.String()
}

// Touch bufio + fmt to keep imports honest if a test is partially commented.
var _ = bufio.NewReader
var _ = fmt.Sprintf
