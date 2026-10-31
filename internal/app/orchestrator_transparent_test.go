package app_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// TestReloadOrchestrator_TransparentListenChangeLogsRestartRequired proves a
// change to transparent.listen emits a restart-required error and does NOT
// rebind the transparent listener mid-flight. Mirrors the socks5/smart tests.
// Covers all three transitions: address change, block added, block removed.
func TestReloadOrchestrator_TransparentListenChangeLogsRestartRequired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		initial     port.Transparent
		newConfig   string
		wantMention string
	}{
		{
			name:        "address changed",
			initial:     port.Transparent{Listen: "127.0.0.1:12345"},
			newConfig:   "transparent:\n  listen: 127.0.0.1:12346\n",
			wantMention: "transparent.listen",
		},
		{
			name:        "block added (was disabled, now enabled)",
			initial:     port.Transparent{Listen: ""},
			newConfig:   "transparent:\n  listen: 127.0.0.1:12345\n",
			wantMention: "transparent.listen",
		},
		{
			name:        "block removed (was enabled, now disabled)",
			initial:     port.Transparent{Listen: "127.0.0.1:12345"},
			newConfig:   "",
			wantMention: "transparent.listen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
			initial := port.Config{
				Listen:      "127.0.0.1:8080",
				Upstream:    port.Upstream{URL: "http://up:3128"},
				Geo:         port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
				Transparent: tt.initial,
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
			if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			loader := yamlconfig.NewLoader()
			errs := make(chan error, 4)
			orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

			orch.Reload()

			select {
			case e := <-errs:
				if e == nil {
					t.Fatal("nil error for transparent.listen change")
				}
				if !contains(e.Error(), "restart") {
					t.Errorf("transparent.listen change error didn't mention restart: %v", e)
				}
				if !contains(e.Error(), tt.wantMention) {
					t.Errorf("transparent.listen change error didn't name the field (%q): %v", tt.wantMention, e)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("expected restart-required error for transparent.listen change")
			}
		})
	}
}

// TestReloadOrchestrator_TransparentListenUnchangedNoError proves that when
// transparent.listen is the same before and after a reload, no
// restart-required error is emitted for it.
func TestReloadOrchestrator_TransparentListenUnchangedNoError(t *testing.T) {
	t.Parallel()

	cfgPtr, rangesPtr, pacPtr, regen := freshSnapshots(t)
	initial := port.Config{
		Listen:      "127.0.0.1:8080",
		Upstream:    port.Upstream{URL: "http://up:3128"},
		Geo:         port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
		Transparent: port.Transparent{Listen: "127.0.0.1:12345"},
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
transparent:
  listen: 127.0.0.1:12345
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loader := yamlconfig.NewLoader()
	errs := make(chan error, 4)
	orch := app.NewReloadOrchestrator(loader, cfgPath, cfgPtr, regen, errs)

	orch.Reload()

	timer := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-errs:
			if e != nil && contains(e.Error(), "transparent.listen") {
				t.Errorf("unexpected transparent.listen restart error for unchanged field: %v", e)
			}
		case <-timer:
			return
		}
	}
}
