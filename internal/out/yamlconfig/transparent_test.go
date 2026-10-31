package yamlconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// baseTransparentYAML is the minimal valid config every transparent case
// layers on.
const baseTransparentYAML = `
listen: 127.0.0.1:8080
upstream:
  url: http://up:3128
geo:
  refresh_interval: 24h
  retry_interval: 5m
`

// loadTransparent writes the given yaml to a temp file and loads it.
func loadTransparent(t *testing.T, yamlText string) port.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yamlText), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loader := yamlconfig.NewLoader()
	cfg, err := loader.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// validateTransparent validates cfg, failing the test when validation errors.
func validateTransparent(t *testing.T, cfg port.Config) {
	t.Helper()
	loader := yamlconfig.NewLoader()
	if err := loader.Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// wantTransparentValidationErr asserts validation fails, wraps
// port.ErrValidation, and names the field.
func wantTransparentValidationErr(t *testing.T, cfg port.Config, field string) {
	t.Helper()
	loader := yamlconfig.NewLoader()
	err := loader.Validate(cfg)
	if err == nil {
		t.Fatalf("expected validation error naming %s, got nil", field)
	}
	if !errors.Is(err, port.ErrValidation) {
		t.Fatalf("error is not port.ErrValidation-wrapped: %v", err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("validation error doesn't name %s: %v", field, err)
	}
}

// TestTransparent_EmptyMeansDisabled proves an absent block and an explicit
// empty value both leave the listener disabled and validate successfully.
func TestTransparent_EmptyMeansDisabled(t *testing.T) {
	t.Parallel()

	t.Run("block absent entirely", func(t *testing.T) {
		cfg := loadTransparent(t, baseTransparentYAML)
		if cfg.Transparent.Listen != "" {
			t.Errorf("Transparent.Listen = %q, want empty", cfg.Transparent.Listen)
		}
		validateTransparent(t, cfg)
	})

	t.Run("explicit empty value", func(t *testing.T) {
		cfg := loadTransparent(t, baseTransparentYAML+"transparent:\n  listen: \"\"\n")
		if cfg.Transparent.Listen != "" {
			t.Errorf("Transparent.Listen = %q, want empty", cfg.Transparent.Listen)
		}
		validateTransparent(t, cfg)
	})
}

// TestTransparent_ValidHostPortParses proves a valid address loads and
// validates.
func TestTransparent_ValidHostPortParses(t *testing.T) {
	t.Parallel()
	cfg := loadTransparent(t, baseTransparentYAML+"transparent:\n  listen: 127.0.0.1:12345\n")
	if cfg.Transparent.Listen != "127.0.0.1:12345" {
		t.Errorf("Transparent.Listen = %q, want 127.0.0.1:12345", cfg.Transparent.Listen)
	}
	validateTransparent(t, cfg)
}

// TestTransparent_InvalidRejected proves malformed addresses are rejected
// with the field named.
func TestTransparent_InvalidRejected(t *testing.T) {
	t.Parallel()

	t.Run("no port", func(t *testing.T) {
		cfg := loadTransparent(t, baseTransparentYAML+"transparent:\n  listen: 127.0.0.1\n")
		wantTransparentValidationErr(t, cfg, "transparent.listen")
	})

	t.Run("not an address at all", func(t *testing.T) {
		cfg := loadTransparent(t, baseTransparentYAML+"transparent:\n  listen: banana\n")
		wantTransparentValidationErr(t, cfg, "transparent.listen")
	})
}

// TestTransparent_NoDefaults proves the loader applies NO activating default:
// an empty value stays empty (the transparent block has no knobs to default,
// unlike smart's tuning fields).
func TestTransparent_NoDefaults(t *testing.T) {
	t.Parallel()
	cfg := loadTransparent(t, baseTransparentYAML)
	if cfg.Transparent.Listen != "" {
		t.Errorf("loader must not default transparent.listen, got %q", cfg.Transparent.Listen)
	}
}
