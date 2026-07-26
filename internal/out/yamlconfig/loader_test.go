package yamlconfig_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoad_AppliesDefaults(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, t.TempDir(), "upstream:\n  url: http://up.example:3128\n")

	l := yamlconfig.NewLoader()
	got, err := l.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen default = %q, want 127.0.0.1:8080", got.Listen)
	}
	if got.Geo.SourceURL != "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest" {
		t.Errorf("SourceURL default = %q", got.Geo.SourceURL)
	}
	if got.Geo.RefreshInterval != 24*time.Hour {
		t.Errorf("RefreshInterval default = %v, want 24h", got.Geo.RefreshInterval)
	}
	if got.Geo.RetryInterval != 5*time.Minute {
		t.Errorf("RetryInterval default = %v, want 5m", got.Geo.RetryInterval)
	}
}

func TestLoad_ExplicitValues(t *testing.T) {
	t.Parallel()
	content := `
listen: 127.0.0.1:9090
upstream:
  url: https://up.example:3128
  username: user
  password: pass
geo:
  source_url: https://example.com/ripe
  refresh_interval: 6h
  retry_interval: 2m
routing:
  whitelist: ["*.a.com", "10.0.0.0/8"]
  blacklist: ["*.b.com"]
`
	p := writeConfig(t, t.TempDir(), content)

	l := yamlconfig.NewLoader()
	got, err := l.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Listen != "127.0.0.1:9090" {
		t.Errorf("Listen = %q", got.Listen)
	}
	if got.Upstream.URL != "https://up.example:3128" {
		t.Errorf("Upstream.URL = %q", got.Upstream.URL)
	}
	if got.Upstream.Username != "user" || got.Upstream.Password != "pass" {
		t.Errorf("creds = %q/%q", got.Upstream.Username, got.Upstream.Password)
	}
	if got.Geo.SourceURL != "https://example.com/ripe" {
		t.Errorf("SourceURL = %q", got.Geo.SourceURL)
	}
	if got.Geo.RefreshInterval != 6*time.Hour {
		t.Errorf("RefreshInterval = %v", got.Geo.RefreshInterval)
	}
	if got.Geo.RetryInterval != 2*time.Minute {
		t.Errorf("RetryInterval = %v", got.Geo.RetryInterval)
	}
	if len(got.Routing.Whitelist) != 2 {
		t.Errorf("Whitelist len = %d", len(got.Routing.Whitelist))
	}
	if len(got.Routing.Blacklist) != 1 || got.Routing.Blacklist[0] != "*.b.com" {
		t.Errorf("Blacklist = %v", got.Routing.Blacklist)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, t.TempDir(), "upstream:\n  url: [unterminated")

	l := yamlconfig.NewLoader()
	_, err := l.Load(p)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	l := yamlconfig.NewLoader()
	_, err := l.Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up.example:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
	}

	l := yamlconfig.NewLoader()

	t.Run("valid config passes", func(t *testing.T) {
		t.Parallel()
		if err := l.Validate(valid); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("https upstream passes", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Upstream.URL = "https://up.example:3128"
		if err := l.Validate(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("bad listen format rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Listen = "not-a-host-port"
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for bad listen")
		}
	})

	t.Run("upstream not http/https rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Upstream.URL = "ftp://up.example"
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for ftp scheme")
		}
	})

	t.Run("upstream missing scheme rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Upstream.URL = "up.example:3128"
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for missing scheme")
		}
	})

	t.Run("non-positive refresh rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Geo.RefreshInterval = 0
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for zero refresh")
		}
	})

	t.Run("non-positive retry rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.Geo.RetryInterval = -time.Second
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for negative retry")
		}
	})
}
