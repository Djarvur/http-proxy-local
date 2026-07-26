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

	// --- Test 3: hot-reload via the FILE WATCHER (production path) ---
	// Edit the config on disk and wait for the fsnotify → debounce →
	// reload → regen → serve loop to deliver the new PAC. We deliberately
	// do NOT call orch.Reload() here — that would test only the orchestrator,
	// not the production hot-reload contract this project exists to deliver.
	// Generous timeout: macOS fsnotify + 100ms debounce + render can take
	// up to ~1s on a loaded CI runner.
	time.Sleep(150 * time.Millisecond) // clear the initial-load debounce window
	writeFile(t, cfgPath, "listen: 127.0.0.1:0\nupstream:\n  url: \"\"\ngeo:\n  source_url: "+stubRipe.URL+"\n  refresh_interval: 24h\n  retry_interval: 5m\nrouting:\n  whitelist: [\"*.added.example.com\"]\n")

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pacResp2, err := http.Get(proxyAddr + "/proxy.pac")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body2, _ := io.ReadAll(pacResp2.Body)
		_ = pacResp2.Body.Close()
		if contains(string(body2), "*.added.example.com") {
			return // PASS — file-watch hot reload worked end-to-end
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("PAC never reflected the file-watch hot reload (fsnotify → reload → regen → serve)")
}

// TestRuntime_HotReload_EditorSafeSave exercises the canonical "safe save"
// pattern that real editors use (vim, emacs, VS Code, etc.): write a sibling
// temp file, fsync it, then atomically rename over the original. This
// triggers a different fsnotify event sequence than in-place writes on macOS
// (FSEvents can deliver Rename+Create rather than Write), so it must be
// tested separately from TestRuntime_Integration which uses os.WriteFile.
//
// Without this test, a regression in the rename-into-path branch of the
// watcher's matches() function would slip through — the editor case is the
// primary real-world save pattern.
func TestRuntime_HotReload_EditorSafeSave(t *testing.T) {
	stubRipe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ripencc|RU|ipv4|1.2.3.0|256|x|x\n"))
	}))
	defer stubRipe.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cachePath := filepath.Join(dir, "ru.cache")
	writeFile(t, cfgPath, "listen: 127.0.0.1:0\nupstream:\n  url: \"\"\ngeo:\n  source_url: "+stubRipe.URL+"\n  refresh_interval: 24h\n  retry_interval: 5m\nrouting:\n  whitelist: [\"before-save.example.com\"]\n")

	loader := yamlconfig.NewLoader()
	cache := fscache.New()
	clock := realClock{}
	src := ripe.New(10 * time.Second)
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

	watcher, err := fsnotify.New(ctx, cfgPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)
	go func() {
		for range watcher.C() {
			orch.Reload()
		}
	}()

	if !waitForPAC(t, pacPtr, 5*time.Second) {
		t.Fatal("initial PAC never generated")
	}

	// Bind + wait for first PAC.
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

	// Confirm the before-save entry is present.
	if !waitForPACContains(t, proxyAddr, "before-save.example.com", 3*time.Second) {
		t.Fatal("initial PAC missing before-save entry")
	}

	// Editor safe-save: write temp in same dir, fsync, rename over original.
	time.Sleep(150 * time.Millisecond) // clear debounce window
	saveLikeEditor(t, cfgPath, "listen: 127.0.0.1:0\nupstream:\n  url: \"\"\ngeo:\n  source_url: "+stubRipe.URL+"\n  refresh_interval: 24h\n  retry_interval: 5m\nrouting:\n  whitelist: [\"after-save.example.com\"]\n")

	if !waitForPACContains(t, proxyAddr, "after-save.example.com", 5*time.Second) {
		t.Fatal("editor safe-save did not propagate to PAC (rename-into-path broken)")
	}

	// Confirm the old entry is gone.
	resp, err := http.Get(proxyAddr + "/proxy.pac")
	if err != nil {
		t.Fatalf("final PAC fetch: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if contains(string(body), "before-save.example.com") {
		t.Error("stale entry remained after editor safe-save reload")
	}
}

func waitForPACContains(t *testing.T, proxyAddr, needle string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(proxyAddr + "/proxy.pac")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if contains(string(body), needle) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// saveLikeEditor writes content to a sibling temp file, fsyncs it, and renames
// it over path — the canonical "safe save" sequence that vim, emacs, VS Code,
// and most modern editors use. This is distinct from os.WriteFile (in-place
// truncate+write) and exercises a different fsnotify event path on macOS.
func saveLikeEditor(t *testing.T, path, content string) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op on success
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		t.Fatalf("write temp: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		t.Fatalf("fsync temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		t.Fatalf("rename temp→%s: %v", path, err)
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
