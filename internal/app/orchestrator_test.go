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

// TestReloadOrchestrator_Socks5ListenChangeLogsRestartRequired proves a
// change to socks5.listen emits a restart-required error and does NOT attempt
// to rebind the SOCKS5 listener mid-flight. Covers all three transitions
// required by the spec: address change, block added, block removed.
func TestReloadOrchestrator_Socks5ListenChangeLogsRestartRequired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		initial    port.SOCKS5
		newConfig  string
		wantMention string // substring the restart-required error must contain
	}{
		{
			name:       "address changed",
			initial:    port.SOCKS5{Listen: "127.0.0.1:1080"},
			newConfig:  "socks5:\n  listen: 127.0.0.1:1085\n",
			wantMention: "socks5.listen",
		},
		{
			name:       "block added (was disabled, now enabled)",
			initial:    port.SOCKS5{Listen: ""}, // disabled
			newConfig:  "socks5:\n  listen: 127.0.0.1:1080\n",
			wantMention: "socks5.listen",
		},
		{
			name:       "block removed (was enabled, now disabled)",
			initial:    port.SOCKS5{Listen: "127.0.0.1:1080"},
			newConfig:  "", // no socks5 block at all
			wantMention: "socks5.listen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
			initial := port.Config{
				Listen:   "127.0.0.1:8080",
				Upstream: port.Upstream{URL: "http://up:3128"},
				Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
				SOCKS5:   tt.initial,
			}
			cfgPtr.Store(&initial)
			stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
			t.Cleanup(stop)

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			// Always include the stable base fields; layer the per-case
			// socks5 block on top.
			content := `
listen: 127.0.0.1:8080
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
` + tt.newConfig
			writeFile(t, cfgPath, content)

			loader := yamlconfig.NewLoader()
			errs := make(chan error, 4)
			orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

			orch.Reload()

			select {
			case e := <-errs:
				if e == nil {
					t.Fatal("nil error for socks5.listen change")
				}
				if !contains(e.Error(), "restart") {
					t.Errorf("socks5.listen change error didn't mention restart: %v", e)
				}
				if !contains(e.Error(), tt.wantMention) {
					t.Errorf("socks5.listen change error didn't name the field (%q): %v", tt.wantMention, e)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("expected restart-required error for socks5.listen change")
			}
		})
	}
}

// TestReloadOrchestrator_Socks5ListenUnchangedNoError proves that when
// socks5.listen is the same before and after a reload, no restart-required
// error is emitted for it. (Guards against false positives — the orchestrator
// must only complain on actual change.)
func TestReloadOrchestrator_Socks5ListenUnchangedNoError(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
		SOCKS5:   port.SOCKS5{Listen: "127.0.0.1:1080"},
	}
	cfgPtr.Store(&initial)
	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Identical socks5.listen before and after — only the (PAC-irrelevant)
	// upstream URL changes, which should NOT trigger a socks5 restart error.
	writeFile(t, cfgPath, `
listen: 127.0.0.1:8080
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
socks5:
  listen: 127.0.0.1:1080
`)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	// No socks5-listen-related error should arrive. We can't assert "channel
	// empty" definitively (other unrelated errors might land), so we drain
	// whatever's there and assert none of them mention socks5.listen.
	timer := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-errs:
			if e != nil && contains(e.Error(), "socks5.listen") {
				t.Errorf("unexpected socks5.listen restart error for unchanged field: %v", e)
			}
		case <-timer:
			return
		}
	}
}

// TestReloadOrchestrator_SmartListenChangeLogsRestartRequired proves a change
// to smart.listen emits a restart-required error and does NOT rebind the smart
// listener mid-flight. Mirrors the socks5.listen test. Covers all three
// transitions: address change, block added, block removed.
func TestReloadOrchestrator_SmartListenChangeLogsRestartRequired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		initial     port.Smart
		newConfig   string
		wantMention string
	}{
		{
			name:        "address changed",
			initial:     port.Smart{Listen: "127.0.0.1:8081"},
			newConfig:   "smart:\n  listen: 127.0.0.1:8082\n",
			wantMention: "smart.listen",
		},
		{
			name:        "block added (was disabled, now enabled)",
			initial:     port.Smart{Listen: ""},
			newConfig:   "smart:\n  listen: 127.0.0.1:8081\n",
			wantMention: "smart.listen",
		},
		{
			name:        "block removed (was enabled, now disabled)",
			initial:     port.Smart{Listen: "127.0.0.1:8081"},
			newConfig:   "",
			wantMention: "smart.listen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
			initial := port.Config{
				Listen:   "127.0.0.1:8080",
				Upstream: port.Upstream{URL: "http://up:3128"},
				Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
				Smart:    tt.initial,
			}
			cfgPtr.Store(&initial)
			stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
			t.Cleanup(stop)

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			content := `
listen: 127.0.0.1:8080
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
` + tt.newConfig
			writeFile(t, cfgPath, content)

			loader := yamlconfig.NewLoader()
			errs := make(chan error, 4)
			orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

			orch.Reload()

			select {
			case e := <-errs:
				if e == nil {
					t.Fatal("nil error for smart.listen change")
				}
				if !contains(e.Error(), "restart") {
					t.Errorf("smart.listen change error didn't mention restart: %v", e)
				}
				if !contains(e.Error(), tt.wantMention) {
					t.Errorf("smart.listen change error didn't name the field (%q): %v", tt.wantMention, e)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("expected restart-required error for smart.listen change")
			}
		})
	}
}

// TestReloadOrchestrator_SmartTuningChangeNoRestartRequired proves that a
// change to the smart TUNING fields (dial_timeout, dial_attempts,
// negative_cache_ttl) does NOT emit a restart-required error — these apply
// per-request with no listener rebind.
func TestReloadOrchestrator_SmartTuningChangeNoRestartRequired(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
		Smart:    port.Smart{Listen: "127.0.0.1:8081", DialTimeout: 3 * time.Second, DialAttempts: 2, NegativeCacheTTL: 60 * time.Second},
	}
	cfgPtr.Store(&initial)
	stop := app.RunPACRegenerator(regen, cfgPtr, rangesPtr, pacPtr)
	t.Cleanup(stop)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Same smart.listen, but the tuning fields change. No restart-required
	// error should mention smart.listen (or smart at all).
	writeFile(t, cfgPath, `
listen: 127.0.0.1:8080
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
smart:
  listen: 127.0.0.1:8081
  dial_timeout: 7s
  dial_attempts: 5
  negative_cache_ttl: 2m
`)

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	timer := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-errs:
			if e != nil && contains(e.Error(), "smart") {
				t.Errorf("unexpected smart-related error for tuning-only change: %v", e)
			}
		case <-timer:
			return
		}
	}
}
