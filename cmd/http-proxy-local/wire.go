package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/out/fscache"
	"github.com/Djarvur/http-proxy-local/internal/out/fsnotify"
	"github.com/Djarvur/http-proxy-local/internal/out/ripe"
	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// configDir is the user's config directory for this binary.
const configDir = ".config/http-proxy-local"

// defaultConfigPath resolves ~/.config/http-proxy-local/config.yaml,
// honoring the XDG-style HOME. Caller may override via the -config flag.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, configDir, "config.yaml"), nil
}

// defaultCachePath resolves ~/.config/http-proxy-local/ru-cidrs.cache.
func defaultCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, configDir, "ru-cidrs.cache"), nil
}

// realClock returns wall time. Implements port.Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Run is the composition root: it loads config, constructs all adapters,
// launches the PAC regenerator + geo fetcher + config watcher + reload
// orchestrator + HTTP server, and blocks until ctx is cancelled. All
// goroutines are cleaned up on return.
//
// This is the single place where the dependency graph is assembled. main()
// is a thin flags+signal shell around this.
func Run(ctx context.Context, configPath string) error {
	if configPath == "" {
		p, err := defaultConfigPath()
		if err != nil {
			return err
		}
		configPath = p
	}
	cachePath, err := defaultCachePath()
	if err != nil {
		return err
	}

	// --- Adapters ---
	loader := yamlconfig.NewLoader()
	cache := fscache.New()
	clock := realClock{}
	src := ripe.New(30 * time.Second)

	// --- Live snapshots ---
	cfgPtr := &atomic.Pointer[port.Config]{}
	rangesPtr := &atomic.Pointer[port.RURanges]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	regen := make(chan struct{}, 8)
	errs := make(chan error, 16)

	// --- Initial config load (blocking; failure to load is fatal) ---
	initial, err := loader.Load(configPath)
	if err != nil {
		return fmt.Errorf("initial config load: %w", err)
	}
	if err := loader.Validate(initial); err != nil {
		return fmt.Errorf("initial config invalid: %w", err)
	}
	cfgPtr.Store(&initial)

	// --- PAC regenerator (single writer to pacPtr) ---
	stopRegen := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	defer stopRegen()

	// --- Geo fetcher (publishes rangesPtr, signals regen) ---
	fetcher := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: initial.Geo.SourceURL, CachePath: cachePath,
		Refresh:   initial.Geo.RefreshInterval,
		Retry:     initial.Geo.RetryInterval,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})
	fetcherCtx, fetcherCancel := context.WithCancel(ctx)
	fetcherDone := make(chan struct{})
	go func() { fetcher.Run(fetcherCtx); close(fetcherDone) }()
	defer func() { fetcherCancel(); <-fetcherDone }()

	// --- Config watcher → reload orchestrator ---
	watcher, err := fsnotify.New(ctx, configPath, 100*time.Millisecond)
	if err != nil {
		return fmt.Errorf("config watcher: %w", err)
	}
	orch := app.NewReloadOrchestrator(loader, configPath, cfgPtr, regen, errs)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		for range watcher.C() {
			orch.Reload()
		}
	}()
	defer func() { <-watcherDone }()

	// --- Error logger (drains errs in background) ---
	errsDone := make(chan struct{})
	go func() {
		defer close(errsDone)
		for e := range errs {
			log.Printf("error: %v", e)
		}
	}()
	// Close errs after all producers stop. Producers are the geo fetcher
	// (stopped via fetcherCancel) and the orchestrator (stopped when watcher
	// closes). We can't precisely coordinate closure here without more
	// plumbing, so leave errs open for the process lifetime. errsDone is
	// drained best-effort on shutdown.

	// --- HTTP server ---
	// Block on the geo fetcher's blocking first-fetch so the first PAC is
	// populated before we serve. The fetcher publishes ranges+regen before
	// Run() returns from startup; we just need to wait for pacPtr.
	firstPACCtx, firstPACCancel := context.WithTimeout(ctx, 60*time.Second)
	defer firstPACCancel()
	if err := waitForFirstPAC(firstPACCtx, pacPtr); err != nil {
		return fmt.Errorf("first PAC never generated: %w", err)
	}

	ln, err := net.Listen("tcp", initial.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", initial.Listen, err)
	}
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second, // mitigates Slowloris (gosec G112)
	}

	// SOCKS5 listener: bind only when configured (design D3 — empty
	// socks5.listen means disabled, full backward compatibility). A bind
	// failure here is fatal, same posture as the HTTP listener.
	var socksLn net.Listener
	if initial.SOCKS5.Listen != "" {
		socksLn, err = net.Listen("tcp", initial.SOCKS5.Listen)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("listen socks5 on %s: %w", initial.SOCKS5.Listen, err)
		}
	}

	// serveBoth blocks until ctx is cancelled OR either listener exits. On
	// exit it shuts both down (design D2: either failing tears down the
	// other — a half-broken process is worse than a clean exit).
	serveErr := serveBoth(ctx, srv, ln, socksLn, cfgPtr)
	_ = errsDone // see comment above about errs being left open
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// waitForFirstPAC polls pacPtr until it's non-nil or ctx is done.
func waitForFirstPAC(ctx context.Context, pacPtr *atomic.Pointer[[]byte]) error {
	for {
		if pacPtr.Load() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
