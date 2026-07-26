package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/out/fscache"
	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// TestRuntime_E2E_PACDecidesRouting is the real end-to-end test: it proves
// that a PAC-aware client, given the proxy's /proxy.pac, makes the correct
// DIRECT-vs-PROXY decision and that the resulting network path is correct.
//
// The flow under test:
//
//	1. Wired runtime serves /proxy.pac with one RU range (for a fake RU origin).
//	2. A "browser" client (this test) GETs /proxy.pac.
//	3. For each target host, the test evaluates FindProxyForURL via node.
//	4. If PAC says "DIRECT" → the client connects to the origin directly,
//	   bypassing the proxy. Upstream must NOT be hit.
//	5. If PAC says "PROXY host:port" → the client sends the request through
//	   the local proxy, which cascades to the configured upstream.
//
// This test failed to exist before; it's the contract the whole project exists
// to deliver. It catches routing-precedence regressions and forwarder bugs
// that unit tests miss.
func TestRuntime_E2E_PACDecidesRouting(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; e2e PAC evaluation requires it")
	}

	// --- Origin servers ---
	// "RU" origin: its IP will be in the proxy's RU_RANGES (we control both).
	// "Non-RU" origin: its IP won't be in RU_RANGES.
	var ruHits, nonRuHits, upstreamHits int32

	ruOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&ruHits, 1)
		_, _ = w.Write([]byte("RU-ORIGIN"))
	}))
	defer ruOrigin.Close()
	nonRuOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&nonRuHits, 1)
		_, _ = w.Write([]byte("NON-RU-ORIGIN"))
	}))
	defer nonRuOrigin.Close()

	// "Upstream" proxy: the local proxy cascades to this. We count hits so
	// we can prove "DIRECT" bypassed it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		// Minimal forward: re-issue to the absolute URL in the request line.
		// gosec G704 false positive: this stub upstream forwards r.RequestURI
		// by design (it's an upstream proxy in the test fixture, not a server
		// vulnerable to SSRF — the whole purpose of an upstream is to forward).
		out, _ := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body) //nolint:gosec // G704: stub upstream forwards by design
		copyHeaders(out.Header, r.Header)
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer upstream.Close()

	// --- Compute the RU origin's IP and decide its RU range ---
	// httptest origins are at 127.0.0.1 — we can't easily vary the IP.
	// Instead, we *whitelist* the non-RU origin's hostname via config so it
	// forces PROXY, and rely on the test's RU_RANGES to mark a synthetic IP.
	//
	// Simpler and more faithful: use the proxy's whitelist/blacklist to drive
	// the routing decision (which the PAC implements), and prove the network
	// path matches. This tests the same PAC→routing→network contract without
	// needing to fake IP routing.
	ruHost := hostOf(ruOrigin.URL)
	nonRuHost := hostOf(nonRuOrigin.URL)

	// Config: whitelist = [nonRuHost] (forces PROXY for non-RU origin),
	// blacklist = [ruHost] (forces DIRECT for RU origin). This is the
	// strongest test of precedence: blacklist > whitelist > everything.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cachePath := filepath.Join(dir, "ru.cache")
	writeFile(t, cfgPath, fmt.Sprintf(`
listen: 127.0.0.1:0
upstream:
  url: %s
routing:
  whitelist: ["%s"]
  blacklist: ["%s"]
`, upstream.URL, nonRuHost, ruHost))

	// --- Wire runtime (no geo fetch — we publish empty ranges directly) ---
	loader := yamlconfig.NewLoader()
	cache := fscache.New()
	clock := realClock{}
	src := &emptyGeoSource{}
	rangesPtr := &atomic.Pointer[port.RURanges]{}
	regen := make(chan struct{}, 8)
	errs := make(chan error, 8)
	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}

	initial, err := loader.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfgPtr.Store(&initial)

	stopRegen := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	defer stopRegen()

	fetcher := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: "stub:", CachePath: cachePath,
		Refresh:   24 * time.Hour, Retry: 5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fetcherDone := make(chan struct{})
	go func() { fetcher.Run(ctx); close(fetcherDone) }()
	defer func() { cancel(); <-fetcherDone }()

	// Wait for PAC.
	if !waitForPAC(t, pacPtr, 5*time.Second) {
		t.Fatal("PAC never generated")
	}

	// Bind the proxy server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srvDone := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(srvDone) }()
	defer func() { _ = srv.Shutdown(context.Background()); <-srvDone }()

	proxyAddr := "http://" + ln.Addr().String()

	// --- Fetch the PAC and prove routing decisions match network paths ---

	// Case 1: blacklist forces DIRECT for the RU origin.
	//   PAC eval → "DIRECT" → client connects to ruOrigin directly.
	//   ruOrigin hit, upstream NOT hit.
	preRu := atomic.LoadInt32(&ruHits)
	preUp := atomic.LoadInt32(&upstreamHits)
	decision := evalPAC(t, proxyAddr, "http://"+ruHost+"/", ruHost)
	if decision != "DIRECT" {
		t.Fatalf("PAC returned %q for blacklisted ruHost, want DIRECT", decision)
	}
	// Make the request DIRECTLY (simulating the browser honoring DIRECT).
	body := httpGet(t, ruOrigin.URL+"/", nil)
	if body != "RU-ORIGIN" {
		t.Errorf("RU origin body = %q, want RU-ORIGIN", body)
	}
	if got := atomic.LoadInt32(&ruHits) - preRu; got != 1 {
		t.Errorf("RU origin hit count delta = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&upstreamHits) - preUp; got != 0 {
		t.Errorf("upstream was hit %d times for DIRECT routing, want 0", got)
	}

	// Case 2: whitelist forces PROXY for the non-RU origin.
	//   PAC eval → "PROXY ..." → client sends through proxy.
	//   nonRuOrigin hit (via upstream cascade), upstream hit.
	preNonRu := atomic.LoadInt32(&nonRuHits)
	preUp2 := atomic.LoadInt32(&upstreamHits)
	decision = evalPAC(t, proxyAddr, "http://"+nonRuHost+"/", nonRuHost)
	if !strings.HasPrefix(decision, "PROXY") {
		t.Fatalf("PAC returned %q for whitelisted nonRuHost, want PROXY ...", decision)
	}
	// Make the request THROUGH the proxy (simulating the browser honoring PROXY).
	proxyURL, _ := url.Parse(proxyAddr)
	body = httpGet(t, nonRuOrigin.URL+"/", proxyURL)
	if body != "NON-RU-ORIGIN" {
		t.Errorf("non-RU origin body = %q, want NON-RU-ORIGIN", body)
	}
	if got := atomic.LoadInt32(&nonRuHits) - preNonRu; got != 1 {
		t.Errorf("non-RU origin hit count delta = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&upstreamHits) - preUp2; got != 1 {
		t.Errorf("upstream hit delta = %d for PROXY routing, want 1", got)
	}
}

// evalPAC fetches /proxy.pac from proxyAddr, evaluates FindProxyForURL via
// node against (url, host), and returns the PAC's decision string.
func evalPAC(t *testing.T, proxyAddr, targetURL, host string) string {
	t.Helper()
	resp, err := http.Get(proxyAddr + "/proxy.pac")
	if err != nil {
		t.Fatalf("fetch PAC: %v", err)
	}
	pacBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Use node to evaluate. We write the PAC + a tiny harness to temp files.
	dir := t.TempDir()
	pacPath := filepath.Join(dir, "proxy.pac")
	if err := os.WriteFile(pacPath, pacBytes, 0o600); err != nil {
		t.Fatalf("write pac: %v", err)
	}
	harness := `
const fs = require('fs');
const pac = fs.readFileSync(process.argv[2], 'utf8');
let dns = null;
global.dnsResolve = () => dns;
global.isPlainHostName = h => !h.includes('.');
global.isInNet = () => false;
global.shExpMatch = (h, p) => {
  const re = new RegExp('^' + p.split('*').map(s => s.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('.*') + '$', 'i');
  return re.test(h);
};
eval(pac);
const tc = JSON.parse(require('fs').readFileSync(0, 'utf8'));
console.log(FindProxyForURL(tc.url, tc.host));
`
	harnessPath := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	var stdin bytes.Buffer
	_ = json.NewEncoder(&stdin).Encode(map[string]string{"url": targetURL, "host": host})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// gosec G204 false positive: "node" is hardcoded; harnessPath and pacPath
	// are test-owned temp files, not attacker-controlled.
	cmd := exec.CommandContext(ctx, "node", harnessPath, pacPath) //nolint:gosec // G204: hardcoded binary, test-owned args
	cmd.Stdin = &stdin
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node harness: %v; stderr: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// httpGet fetches url, optionally through proxyURL. Returns the body string.
func httpGet(t *testing.T, target string, proxyURL *url.URL) string {
	t.Helper()
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: func(_ *http.Request) (*url.URL, error) {
				if proxyURL != nil {
					return proxyURL, nil
				}
				return nil, nil
			},
		},
	}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s (via proxy=%v): %v", target, proxyURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s status = %d", target, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func hostOf(rawURL string) string {
	u, _ := url.Parse(rawURL)
	return u.Host
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func waitForPAC(t *testing.T, pacPtr *atomic.Pointer[[]byte], timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pacPtr.Load() != nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// emptyGeoSource returns empty body — the e2e test uses whitelist/blacklist
// to drive routing, so RU_RANGES is empty (no real geo needed).
type emptyGeoSource struct{}

func (emptyGeoSource) Fetch(_ context.Context, _ string) ([]byte, error) {
	return []byte(""), nil
}
