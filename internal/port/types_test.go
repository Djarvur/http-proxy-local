package port_test

import (
	"testing"
	"time"

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
