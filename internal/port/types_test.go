package port_test

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

func TestConfigDefaultsAndFields(t *testing.T) {
	t.Parallel()

	c := port.Config{
		Listen: "127.0.0.1:8080",
		Upstream: port.Upstream{
			URL:      "http://upstream.example:3128",
			Username: "u",
			Password: "p",
		},
		Geo: port.Geo{
			SourceURL:       "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest",
			RefreshInterval: 24 * time.Hour,
			RetryInterval:   5 * time.Minute,
		},
		Routing: port.Routing{
			Whitelist: []string{"*.a.com"},
			Blacklist: []string{"*.b.com"},
		},
	}

	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want 127.0.0.1:8080", c.Listen)
	}
	if c.Upstream.URL != "http://upstream.example:3128" {
		t.Errorf("Upstream.URL = %q", c.Upstream.URL)
	}
	if c.Upstream.Username != "u" || c.Upstream.Password != "p" {
		t.Errorf("Upstream creds = %q/%q", c.Upstream.Username, c.Upstream.Password)
	}
	if c.Geo.RefreshInterval != 24*time.Hour {
		t.Errorf("RefreshInterval = %v", c.Geo.RefreshInterval)
	}
	if c.Geo.RetryInterval != 5*time.Minute {
		t.Errorf("RetryInterval = %v", c.Geo.RetryInterval)
	}
	if len(c.Routing.Whitelist) != 1 || c.Routing.Whitelist[0] != "*.a.com" {
		t.Errorf("Whitelist = %v", c.Routing.Whitelist)
	}
	if len(c.Routing.Blacklist) != 1 || c.Routing.Blacklist[0] != "*.b.com" {
		t.Errorf("Blacklist = %v", c.Routing.Blacklist)
	}
}

func TestSentinelErrorsExist(t *testing.T) {
	t.Parallel()

	errs := map[string]error{
		"ErrCacheMiss":     port.ErrCacheMiss,
		"ErrCacheStale":    port.ErrCacheStale,
		"ErrParseFailed":   port.ErrParseFailed,
		"ErrValidation": port.ErrValidation,
	}
	for name, e := range errs {
		if e == nil {
			t.Errorf("sentinel %s is nil", name)
		}
	}
}

func TestPACInputAndRanges(t *testing.T) {
	t.Parallel()

	r := port.RURanges{Ranges: []port.Range{{Low: 10, High: 20}, {Low: 30, High: 40}}}
	if len(r.Ranges) != 2 {
		t.Fatalf("RURanges len = %d", len(r.Ranges))
	}
	if r.Ranges[0].Low != 10 || r.Ranges[0].High != 20 {
		t.Errorf("Range[0] = %+v", r.Ranges[0])
	}

	in := port.PACInput{
		Listen:    "127.0.0.1:8080",
		Whitelist: []string{"*.a.com"},
		Blacklist: []string{"*.b.com"},
		Ranges:    r,
	}
	if in.Listen != "127.0.0.1:8080" {
		t.Errorf("PACInput.Listen = %q", in.Listen)
	}
}

// TestPAC_EffectiveCacheSeconds locks in the omitted-vs-explicit-zero
// distinction. The field is a pointer so the loader can tell "field absent"
// (nil → default applies) from "operator wrote cache_seconds: 0" (explicit
// opt-out of caching). This is the one field that breaks the "zero means
// default" convention used elsewhere; it's documented on the type.
func TestPAC_EffectiveCacheSeconds(t *testing.T) {
	t.Parallel()

	const defaultSec = 15

	tests := []struct {
		name string
		pac  port.PAC
		want int
	}{
		{name: "nil pointer (omitted) yields default", pac: port.PAC{CacheSeconds: nil}, want: defaultSec},
		{name: "explicit zero means no-cache", pac: port.PAC{CacheSeconds: intPtr(0)}, want: 0},
		{name: "explicit positive passes through", pac: port.PAC{CacheSeconds: intPtr(30)}, want: 30},
		{name: "explicit one passes through", pac: port.PAC{CacheSeconds: intPtr(1)}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.pac.EffectiveCacheSeconds(defaultSec); got != tt.want {
				t.Errorf("EffectiveCacheSeconds(%d) = %d, want %d", defaultSec, got, tt.want)
			}
		})
	}
}

func intPtr(i int) *int { return &i }

// TestSOCKS5_YAMLRoundTrip locks in the Config.SOCKS5 field's YAML behavior:
// a non-empty socks5.listen survives marshal+unmarshal, and an absent socks5
// block parses to SOCKS5{Listen: ""} (the disabled state). Per design D3
// there is no activating default port — empty means disabled, full stop.
func TestSOCKS5_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("non-empty listen round-trips", func(t *testing.T) {
		t.Parallel()
		in := port.Config{SOCKS5: port.SOCKS5{Listen: "127.0.0.1:1080"}}
		raw, err := yaml.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out port.Config
		if err := yaml.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.SOCKS5.Listen != "127.0.0.1:1080" {
			t.Errorf("SOCKS5.Listen = %q, want 127.0.0.1:1080", out.SOCKS5.Listen)
		}
	})

	t.Run("absent socks5 block leaves Listen empty", func(t *testing.T) {
		t.Parallel()
		// Minimal config with no socks5: block at all.
		const doc = "listen: 127.0.0.1:8080\n"
		var out port.Config
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.SOCKS5.Listen != "" {
			t.Errorf("SOCKS5.Listen = %q, want empty (disabled)", out.SOCKS5.Listen)
		}
	})

	t.Run("explicit empty socks5.listen leaves Listen empty", func(t *testing.T) {
		t.Parallel()
		const doc = "socks5:\n  listen: \"\"\n"
		var out port.Config
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.SOCKS5.Listen != "" {
			t.Errorf("SOCKS5.Listen = %q, want empty (operator opt-out)", out.SOCKS5.Listen)
		}
	})

	// Sanity: ensure the YAML key is the snake-case `socks5.listen` (not the
	// Go field name), so the documented config schema matches.
	t.Run("documented snake-case keys", func(t *testing.T) {
		t.Parallel()
		in := port.Config{SOCKS5: port.SOCKS5{Listen: "127.0.0.1:1080"}}
		raw, err := yaml.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), "socks5:") {
			t.Errorf("marshaled YAML missing 'socks5:' key; got:\n%s", raw)
		}
		if !strings.Contains(string(raw), "listen:") {
			t.Errorf("marshaled YAML missing 'listen:' key; got:\n%s", raw)
		}
	})
}

// TestSmart_YAMLRoundTrip locks in the Config.Smart field's YAML behavior: a
// populated smart block survives marshal+unmarshal with all four fields
// preserved, and an absent smart block parses to the zero value (disabled).
// Per design D8 there is no activating default port — empty Listen means
// disabled, full stop. The three tuning fields default when zero (applied by
// the loader, not by the struct), so their zero value is also a valid "unset".
func TestSmart_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("populated block round-trips", func(t *testing.T) {
		t.Parallel()
		in := port.Config{Smart: port.Smart{
			Listen:           "127.0.0.1:8081",
			DialTimeout:      3 * time.Second,
			DialAttempts:     2,
			NegativeCacheTTL: 60 * time.Second,
		}}
		raw, err := yaml.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out port.Config
		if err := yaml.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Smart.Listen != "127.0.0.1:8081" {
			t.Errorf("Smart.Listen = %q, want 127.0.0.1:8081", out.Smart.Listen)
		}
		if out.Smart.DialTimeout != 3*time.Second {
			t.Errorf("Smart.DialTimeout = %v, want 3s", out.Smart.DialTimeout)
		}
		if out.Smart.DialAttempts != 2 {
			t.Errorf("Smart.DialAttempts = %d, want 2", out.Smart.DialAttempts)
		}
		if out.Smart.NegativeCacheTTL != 60*time.Second {
			t.Errorf("Smart.NegativeCacheTTL = %v, want 60s", out.Smart.NegativeCacheTTL)
		}
	})

	t.Run("absent smart block leaves all fields zero", func(t *testing.T) {
		t.Parallel()
		const doc = "listen: 127.0.0.1:8080\n"
		var out port.Config
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Smart.Listen != "" {
			t.Errorf("Smart.Listen = %q, want empty (disabled)", out.Smart.Listen)
		}
		if out.Smart.DialTimeout != 0 {
			t.Errorf("Smart.DialTimeout = %v, want 0 (unset)", out.Smart.DialTimeout)
		}
		if out.Smart.DialAttempts != 0 {
			t.Errorf("Smart.DialAttempts = %d, want 0 (unset)", out.Smart.DialAttempts)
		}
		if out.Smart.NegativeCacheTTL != 0 {
			t.Errorf("Smart.NegativeCacheTTL = %v, want 0 (unset)", out.Smart.NegativeCacheTTL)
		}
	})

	t.Run("explicit empty smart.listen leaves Listen empty", func(t *testing.T) {
		t.Parallel()
		const doc = "smart:\n  listen: \"\"\n"
		var out port.Config
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Smart.Listen != "" {
			t.Errorf("Smart.Listen = %q, want empty (operator opt-out)", out.Smart.Listen)
		}
	})

	t.Run("documented snake-case keys", func(t *testing.T) {
		t.Parallel()
		in := port.Config{Smart: port.Smart{Listen: "127.0.0.1:8081"}}
		raw, err := yaml.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), "smart:") {
			t.Errorf("marshaled YAML missing 'smart:' key; got:\n%s", raw)
		}
	})
}
