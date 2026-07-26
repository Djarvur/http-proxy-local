package main_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/out/fscache"
	"github.com/Djarvur/http-proxy-local/internal/out/fsnotify"
	"github.com/Djarvur/http-proxy-local/internal/out/ripe"
	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// TestRuntime_Integration exercises the fully-wired runtime against a stub
// RIPE server and a stub upstream origin. It asserts the three end-to-end
// contracts: PAC served, request forwarded through to origin, hot-reload
// regenerates the PAC.
func TestRuntime_Integration(t *testing.T) {
	// Not parallel — uses real fsnotify + http.ListenAndServe.

	// --- Stubs ---
	stubRipe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ripencc|RU|ipv4|1.2.3.0|256|x|x\n"))
	}))
	defer stubRipe.Close()

	var originSaw int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&originSaw, 1)
		_, _ = w.Write([]byte("hello-from-origin"))
	}))
	defer origin.Close()

	// --- Config dir ---
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cachePath := filepath.Join(dir, "ru-cidrs.cache")
	writeFile(t, cfgPath, "listen: 127.0.0.1:0\nupstream:\n  url: \"\"\ngeo:\n  source_url: "+stubRipe.URL+"\n  refresh_interval: 24h\n  retry_interval: 5m\n")

	// --- Wire adapters ---
	loader := yamlconfig.NewLoader()
	cache := fscache.New()
	clock := realClock{}
	src := ripe.New(10 * time.Second)
	rangesPtr := &atomic.Pointer[port.RURanges]{}
	regen := make(chan struct{}, 8)
	errs := make(chan error, 8)

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}

	// Seed initial config (listen is dummy; we won't actually bind).
	initial, err := loader.Load(cfgPath)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	cfgPtr.Store(&initial)

	// PAC regenerator.
	stopRegen := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	defer stopRegen()

	// Geo fetcher.
	fetcher := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: initial.Geo.SourceURL, CachePath: cachePath,
		Refresh:   initial.Geo.RefreshInterval,
		Retry:     initial.Geo.RetryInterval,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fetcherDone := make(chan struct{})
	go func() { fetcher.Run(ctx); close(fetcherDone) }()
	defer func() { cancel(); <-fetcherDone }()

	// Config watcher (debounced) → reload orchestrator.
	watcher, err := fsnotify.New(ctx, cfgPath, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)
	go func() {
		for range watcher.C() {
			orch.Reload()
		}
	}()

	// --- Wait for the geo fetch to publish ranges + PAC regen ---
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pacPtr.Load() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pacPtr.Load() == nil {
		t.Fatal("PAC never generated")
	}

	// --- Bind a real HTTP server on the configured listen addr ---
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second, // gosec G112
	}
	srvDone := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(srvDone) }()
	defer func() { _ = srv.Shutdown(context.Background()); <-srvDone }()

	proxyAddr := "http://" + ln.Addr().String()

	// --- Test 1: PAC is served ---
	pacResp, err := http.Get(proxyAddr + "/proxy.pac")
	if err != nil {
		t.Fatalf("GET /proxy.pac: %v", err)
	}
	body, _ := io.ReadAll(pacResp.Body)
	_ = pacResp.Body.Close()
	if pacResp.StatusCode != 200 {
		t.Errorf("PAC status = %d", pacResp.StatusCode)
	}
	if len(body) == 0 {
		t.Error("PAC body is empty")
	}

	// --- Test 2: forwarded request reaches origin ---
	// Point the proxy's "upstream" at the origin by editing config. Upstream
	// URL is "" (direct), so requests go direct — we just need the origin
	// URL to be reachable. Send a plain HTTP request through the proxy.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(_ *http.Request) (*url.URL, error) {
				return url.Parse(proxyAddr)
			},
		},
	}
	resp, err := client.Get(origin.URL + "/path")
	if err != nil {
		t.Fatalf("proxied GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if string(rb) != "hello-from-origin" {
		t.Errorf("origin response = %q", rb)
	}
	if got := atomic.LoadInt32(&originSaw); got != 1 {
		t.Errorf("origin saw %d requests, want 1", got)
	}

	// --- Test 3: hot-reload regenerates PAC ---
	// Write the new config, then trigger a reload directly (the fsnotify
	// path is proven in G8's dedicated watcher tests; here we verify the
	// reload→regen→serve wiring).
	time.Sleep(150 * time.Millisecond)
	writeFile(t, cfgPath, "listen: 127.0.0.1:0\nupstream:\n  url: \"\"\ngeo:\n  source_url: "+stubRipe.URL+"\n  refresh_interval: 24h\n  retry_interval: 5m\nrouting:\n  whitelist: [\"*.added.example.com\"]\n")

	// Drain any prior errors before the explicit reload below.
	drainErrors(errs)
	orch.Reload()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pacResp2, err := http.Get(proxyAddr + "/proxy.pac")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body2, _ := io.ReadAll(pacResp2.Body)
		_ = pacResp2.Body.Close()
		if contains(string(body2), "*.added.example.com") {
			return // PASS — hot-reload worked
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("PAC never reflected hot-reloaded whitelist entry")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// realClock returns the actual wall time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func drainErrors(ch chan error) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
