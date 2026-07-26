// Package yamlconfig implements port.ConfigLoader: it reads, parses, applies
// defaults to, and validates the proxy's config.yaml. It is the only place
// that knows the on-disk YAML schema.
package yamlconfig

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// Defaults applied when a field is omitted from the YAML. These mirror the
// values documented in the proposal and design.
const (
	defaultListen   = "127.0.0.1:8080"
	defaultSource   = "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest"
	defaultRefresh  = 24 * time.Hour
	defaultRetry    = 5 * time.Minute
)

// Loader implements port.ConfigLoader. It has no state; the type exists so
// callers depend on the interface, not the package directly.
type Loader struct{}

// NewLoader returns a Loader.
func NewLoader() Loader { return Loader{} }

// Load reads, parses, and applies defaults to the config at path.
// Validation is NOT applied here — callers should run Validate separately
// (or chain Load + Validate) so the two failure modes are distinguishable.
func (Loader) Load(path string) (port.Config, error) {
	// gosec G304 is a false positive: path is operator-supplied (via -config
	// flag or the documented default), not attacker-controlled. There is no
	// way to implement a config loader without reading a path argument.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return port.Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	// Parse into a yaml-node-aware struct so we can detect "field was absent"
	// vs "field was present-but-zero" when applying defaults. For simplicity
	// we parse into the typed Config (yaml leaves zero values for absent
	// fields) and then layer defaults onto zero values.
	var c port.Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return port.Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	applyDefaults(&c)
	return c, nil
}

// Validate enforces semantic constraints and returns a port.ErrValidation-
// wrapped error describing the first failure encountered.
func (Loader) Validate(c port.Config) error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("%w: listen %q is not host:port: %w", port.ErrValidation, c.Listen, err)
	}

	// An empty upstream URL means "no upstream, connect direct" (useful for
	// local testing and for deployments where only the PAC matters). Only
	// validate the scheme/host when a URL is actually set.
	if c.Upstream.URL != "" {
		u, err := url.Parse(c.Upstream.URL)
		if err != nil {
			return fmt.Errorf("%w: upstream.url parse: %w", port.ErrValidation, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%w: upstream.url scheme must be http or https, got %q", port.ErrValidation, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("%w: upstream.url missing host", port.ErrValidation)
		}
	}

	if c.Geo.RefreshInterval <= 0 {
		return fmt.Errorf("%w: geo.refresh_interval must be positive, got %v", port.ErrValidation, c.Geo.RefreshInterval)
	}
	if c.Geo.RetryInterval <= 0 {
		return fmt.Errorf("%w: geo.retry_interval must be positive, got %v", port.ErrValidation, c.Geo.RetryInterval)
	}

	// pac.cache_seconds: nil (omitted) and 0 (explicit opt-out) are both
	// valid. Only negative values are rejected.
	if c.PAC.CacheSeconds != nil && *c.PAC.CacheSeconds < 0 {
		return fmt.Errorf("%w: pac.cache_seconds must be non-negative when set, got %d", port.ErrValidation, *c.PAC.CacheSeconds)
	}

	return nil
}

// applyDefaults fills in zero-valued fields with their documented defaults.
// Note: this intentionally treats "explicitly set to zero" the same as
// "omitted" — zero durations and empty strings are not useful values for
// these fields, so defaulting them is safe.
//
// For upstream credentials: if Username/Password are empty but the URL
// contains user:pass@, we extract them from the URL. This supports the
// common "credentials embedded in the URL" form (e.g.
// https://user:pass@host:port). The URL is left unchanged — downstream
// code reads the parsed Username/Password fields, not the URL.
func applyDefaults(c *port.Config) {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.Geo.SourceURL == "" {
		c.Geo.SourceURL = defaultSource
	}
	if c.Geo.RefreshInterval == 0 {
		c.Geo.RefreshInterval = defaultRefresh
	}
	if c.Geo.RetryInterval == 0 {
		c.Geo.RetryInterval = defaultRetry
	}
	extractUpstreamCreds(c)
}

// extractUpstreamCreds populates Upstream.Username/Password from user:pass@
// in the URL when those fields are empty. No-op if the URL has no creds or
// the fields are already set (explicit fields win over URL-embedded ones).
func extractUpstreamCreds(c *port.Config) {
	if c.Upstream.URL == "" {
		return
	}
	if c.Upstream.Username != "" || c.Upstream.Password != "" {
		return
	}
	u, err := url.Parse(c.Upstream.URL)
	if err != nil || u.User == nil {
		return
	}
	c.Upstream.Username = u.User.Username()
	if p, ok := u.User.Password(); ok {
		c.Upstream.Password = p
	}
}
