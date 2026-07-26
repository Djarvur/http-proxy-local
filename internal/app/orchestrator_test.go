package app_test

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// freshSnapshots returns empty atomic pointers for config, ranges, and pac,
// plus the regenerator's signal channel.
func freshSnapshots(t *testing.T) (
	cfgPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	pacPtr *atomic.Pointer[[]byte],
	regen chan struct{},
) {
	t.Helper()
	cfgPtr = &atomic.Pointer[port.Config]{}
	rangesPtr = &atomic.Pointer[port.RURanges]{}
	pacPtr = &atomic.Pointer[[]byte]{}
	regen = make(chan struct{}, 8)
	return
}

func TestPACRegenerator_RendersOnSignal(t *testing.T) {
	t.Parallel()
	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)

	// Seed a config + ranges so the renderer has something to render.
	cfg := port.Config{Listen: "127.0.0.1:8080"}
	cfgPtr.Store(&cfg)
	ranges := port.RURanges{Ranges: []port.Range{{Low: 1, High: 2}}}
	rangesPtr.Store(&ranges)

	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	regen <- struct{}{}

	// The regenerator should render and store within a short window.
	waitFor(t, pacPtr, "127.0.0.1:8080")
}

func TestPACRegenerator_CoalescesRapidSignals(t *testing.T) {
	t.Parallel()
	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)

	cfg := port.Config{Listen: "127.0.0.1:9090"}
	cfgPtr.Store(&cfg)
	rangesPtr.Store(&port.RURanges{})

	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	// Fire several rapid signals.
	for i := 0; i < 5; i++ {
		regen <- struct{}{}
	}

	// The eventual PAC must reflect the latest config, and the regenerator
	// must not deadlock on a full-ish channel. We can't easily assert "only
	// one render happened" without instrumentation, but the contract is:
	// one final consistent PAC exists.
	waitFor(t, pacPtr, "127.0.0.1:9090")
}

func TestPACRegenerator_PicksUpRangesUpdate(t *testing.T) {
	t.Parallel()
	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	cfgPtr.Store(&port.Config{Listen: "127.0.0.1:8080"})

	// Initial ranges.
	r1 := port.RURanges{Ranges: []port.Range{{Low: 10, High: 20}}}
	rangesPtr.Store(&r1)

	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)
	regen <- struct{}{}
	waitFor(t, pacPtr, "[10, 20]")

	// Update ranges and re-signal.
	r2 := port.RURanges{Ranges: []port.Range{{Low: 30, High: 40}}}
	rangesPtr.Store(&r2)
	regen <- struct{}{}
	waitFor(t, pacPtr, "[30, 40]")
}

// waitFor polls pacPtr until the rendered body contains `want`, failing the
// test after a deadline. The regenerator runs in its own goroutine and the
// store may take a few ms to land.
func waitFor(t *testing.T, pacPtr *atomic.Pointer[[]byte], want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := pacPtr.Load(); p != nil && contains(string(*p), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	p := pacPtr.Load()
	body := "<nil>"
	if p != nil {
		body = string(*p)
	}
	t.Fatalf("PAC never contained %q; last body:\n%s", want, body)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestReloadOrchestrator_ValidSwapUpdatesConfigAndSignalsPACFields(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	// Seed initial config.
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
	}
	cfgPtr.Store(&initial)

	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	// Write a config that changes a PAC-affecting field (whitelist) while
	// keeping listen stable (so no restart-required error).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	newCfg := `
listen: 127.0.0.1:8080
upstream:
  url: http://new-up:3128
routing:
  whitelist: ["*.added.com"]
geo:
  refresh_interval: 24h
  retry_interval: 5m
`
	writeFile(t, cfgPath, newCfg)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	// Config should reflect the new upstream + whitelist.
	got := cfgPtr.Load()
	if got == nil {
		t.Fatalf("config not swapped")
	}
	if got.Upstream.URL != "http://new-up:3128" {
		t.Errorf("Upstream.URL = %q", got.Upstream.URL)
	}
	if len(got.Routing.Whitelist) != 1 || got.Routing.Whitelist[0] != "*.added.com" {
		t.Errorf("Whitelist = %v", got.Routing.Whitelist)
	}

	// PAC should have been regenerated (whitelist changed).
	waitFor(t, pacPtr, "*.added.com")

	// No validation or restart-required error.
	select {
	case e := <-errs:
		t.Fatalf("unexpected error: %v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReloadOrchestrator_UpstreamOnlyChangeDoesNotSignalPAC(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://old-up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
	}
	cfgPtr.Store(&initial)

	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)
	// Pre-render one PAC so the regenerator isn't idle.
	regen <- struct{}{}
	waitFor(t, pacPtr, "127.0.0.1:8080")

	// Drain any residual signals.
	drainSignals(regen)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Only upstream.url changes — PAC-affecting fields are identical.
	newCfg := `
listen: 127.0.0.1:8080
upstream:
  url: http://new-up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
`
	writeFile(t, cfgPath, newCfg)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	// Config reflects the new upstream.
	if got := cfgPtr.Load(); got == nil || got.Upstream.URL != "http://new-up:3128" {
		t.Fatalf("upstream not updated; got %+v", got)
	}

	// No PAC regen signal should arrive within a grace window.
	select {
	case <-regen:
		t.Fatal("PAC was signaled despite only upstream.url changing")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReloadOrchestrator_InvalidConfigRetainsOld(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
	}
	cfgPtr.Store(&initial)
	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Invalid: upstream.url is ftp.
	writeFile(t, cfgPath, `
listen: 127.0.0.1:9090
upstream:
  url: ftp://bad
geo:
  refresh_interval: 24h
  retry_interval: 5m
`)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	// Config must be unchanged.
	if got := cfgPtr.Load(); got == nil || got.Listen != "127.0.0.1:8080" {
		t.Fatalf("config was mutated by invalid reload; got %+v", got)
	}

	// An error must be logged.
	select {
	case <-errs:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected an error for invalid config, got none")
	}
}

func TestReloadOrchestrator_ListenChangeLogsRestartRequired(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
	}
	cfgPtr.Store(&initial)
	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `
listen: 127.0.0.1:9090
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
`)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	// listen change must log a restart-required error.
	select {
	case e := <-errs:
		if e == nil {
			t.Fatal("nil error for listen change")
		}
		// The error should mention restart.
		if !contains(e.Error(), "restart") {
			t.Errorf("listen-change error didn't mention restart: %v", e)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected restart-required error for listen change")
	}

	// The listen field IS swapped (the config snapshot is updated), but the
	// listener won't be rebound — that's the caller's responsibility. The
	// error is the signal that rebind didn't happen.
	if got := cfgPtr.Load(); got == nil || got.Listen != "127.0.0.1:9090" {
		t.Fatalf("config listen not updated: %+v", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func drainSignals(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
