package yamlconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// Omitted pac: → EffectiveCacheSeconds returns the default (15). The
	// pointer itself stays nil (we don't synthesize one in applyDefaults —
	// nil IS the "use default" signal, preserved through to the consumer).
	if got := got.PAC.EffectiveCacheSeconds(port.DefaultPACCacheSeconds); got != port.DefaultPACCacheSeconds {
		t.Errorf("PAC default EffectiveCacheSeconds = %d, want %d", got, port.DefaultPACCacheSeconds)
	}
	if got.PAC.CacheSeconds != nil {
		t.Errorf("PAC.CacheSeconds = %v, want nil (omitted → not synthesized)", got.PAC.CacheSeconds)
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
pac:
  cache_seconds: 30
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
	if got.PAC.CacheSeconds == nil || *got.PAC.CacheSeconds != 30 {
		t.Errorf("PAC.CacheSeconds = %v, want pointer to 30", got.PAC.CacheSeconds)
	}
}

// TestLoad_Upstream_CredsExtractedFromURL verifies the common "credentials
// embedded in URL" form is supported. When upstream.url contains
// user:pass@host (and username/password fields are empty), the loader MUST
// populate the Username/Password fields from the URL — otherwise the
// forwarder and CONNECT tunnel paths silently send no Proxy-Authorization,
// and the upstream returns 407. This was a real production bug discovered
// during deployment smoke testing.
func TestLoad_Upstream_CredsExtractedFromURL(t *testing.T) {
	t.Parallel()

	t.Run("creds in URL populate fields", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, t.TempDir(), "upstream:\n  url: https://alice:secret@host:3128\n")
		l := yamlconfig.NewLoader()
		got, err := l.Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Upstream.Username != "alice" {
			t.Errorf("Username = %q, want alice", got.Upstream.Username)
		}
		if got.Upstream.Password != "secret" {
			t.Errorf("Password = %q, want secret", got.Upstream.Password)
		}
		// URL itself stays unchanged (downstream code does not re-parse it).
		// gosec G101 false positive: hardcoded creds in a test fixture URL.
		if got.Upstream.URL != "https://alice:secret@host:3128" { //nolint:gosec // G101: test fixture
			t.Errorf("URL was modified: %q", got.Upstream.URL)
		}
	})

	t.Run("explicit fields win over URL creds", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, t.TempDir(), "upstream:\n  url: https://urluser:urlpass@host:3128\n  username: fielduser\n  password: fieldpass\n")
		l := yamlconfig.NewLoader()
		got, err := l.Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Upstream.Username != "fielduser" {
			t.Errorf("Username = %q, want fielduser (explicit field must win)", got.Upstream.Username)
		}
		if got.Upstream.Password != "fieldpass" {
			t.Errorf("Password = %q, want fieldpass (explicit field must win)", got.Upstream.Password)
		}
	})

	t.Run("URL without creds leaves fields empty", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, t.TempDir(), "upstream:\n  url: https://host:3128\n")
		l := yamlconfig.NewLoader()
		got, err := l.Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Upstream.Username != "" || got.Upstream.Password != "" {
			t.Errorf("creds should be empty, got %q/%q", got.Upstream.Username, got.Upstream.Password)
		}
	})
}

// TestLoad_PAC_CacheSeconds_ZeroIsExplicit verifies the omitted-vs-zero
// distinction survives the YAML round-trip. cache_seconds: 0 is the
// operator's explicit opt-out of caching (→ no-cache header); it must NOT
// be silently replaced with the default.
func TestLoad_PAC_CacheSeconds_ZeroIsExplicit(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, t.TempDir(), `
upstream: {url: ""}
pac:
  cache_seconds: 0
`)
	l := yamlconfig.NewLoader()
	got, err := l.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PAC.CacheSeconds == nil {
		t.Fatal("CacheSeconds = nil, want non-nil (explicit 0 should round-trip as &0)")
	}
	if *got.PAC.CacheSeconds != 0 {
		t.Errorf("CacheSeconds = %d, want 0 (explicit opt-out)", *got.PAC.CacheSeconds)
	}
	if eff := got.PAC.EffectiveCacheSeconds(port.DefaultPACCacheSeconds); eff != 0 {
		t.Errorf("EffectiveCacheSeconds = %d, want 0 (must not default explicit zero)", eff)
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

	zero := 0
	valid := port.Config{
		Listen:   "127.0.0.1:8080",
		Upstream: port.Upstream{URL: "http://up.example:3128"},
		Geo:      port.Geo{RefreshInterval: 24 * time.Hour, RetryInterval: 5 * time.Minute},
		PAC:      port.PAC{CacheSeconds: &zero},
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

	t.Run("negative pac.cache_seconds rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		neg := -1
		c.PAC.CacheSeconds = &neg
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for negative pac.cache_seconds")
		}
	})

	t.Run("zero pac.cache_seconds accepted", func(t *testing.T) {
		t.Parallel()
		c := valid
		zero := 0
		c.PAC.CacheSeconds = &zero
		if err := l.Validate(c); err != nil {
			t.Fatalf("zero pac.cache_seconds should be valid (no-cache opt-out): %v", err)
		}
	})

	t.Run("nil pac.cache_seconds accepted", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.PAC.CacheSeconds = nil
		if err := l.Validate(c); err != nil {
			t.Fatalf("nil pac.cache_seconds should be valid (defaults applied later): %v", err)
		}
	})

	t.Run("valid socks5.listen accepted", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.SOCKS5.Listen = "127.0.0.1:1080"
		if err := l.Validate(c); err != nil {
			t.Fatalf("valid socks5.listen should pass: %v", err)
		}
	})

	t.Run("empty socks5.listen accepted as disabled", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.SOCKS5.Listen = "" // absent block OR explicit empty both parse to ""
		if err := l.Validate(c); err != nil {
			t.Fatalf("empty socks5.listen (disabled) should be valid: %v", err)
		}
	})

	t.Run("malformed socks5.listen rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.SOCKS5.Listen = "not-a-host-port"
		err := l.Validate(c)
		if err == nil {
			t.Fatal("expected error for malformed socks5.listen")
		}
		if !errors.Is(err, port.ErrValidation) {
			t.Errorf("error should wrap port.ErrValidation, got: %v", err)
		}
		// The message must name the field so the operator knows which knob to fix.
		if !strings.Contains(err.Error(), "socks5.listen") {
			t.Errorf("error should name 'socks5.listen', got: %v", err)
		}
	})

	t.Run("socks5.listen missing port rejected", func(t *testing.T) {
		t.Parallel()
		c := valid
		c.SOCKS5.Listen = "127.0.0.1" // host without :port
		if err := l.Validate(c); err == nil {
			t.Fatal("expected error for socks5.listen without port")
		}
	})
}
