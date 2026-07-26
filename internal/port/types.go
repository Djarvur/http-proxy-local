// Package port defines the boundary types and contracts of the http-proxy-local
// application. It is the single shared vocabulary between the app layer
// (internal/app) and its adapters (internal/in, internal/out).
//
// Per go-ultimate's Hexagonal layout: this package contains interfaces and
// DTOs only — no business logic, no I/O. Adapters implement the Out ports
// defined here; the app layer depends on them.
package port

import (
	"errors"
	"time"
)

// Config is the fully-parsed, validated configuration snapshot. Instances are
// published atomically by the config-reload goroutine and read per-request by
// the proxy handler and PAC generator.
type Config struct {
	Listen   string   `yaml:"listen"`
	Upstream Upstream `yaml:"upstream"`
	Geo      Geo      `yaml:"geo"`
	Routing  Routing  `yaml:"routing"`
	PAC      PAC      `yaml:"pac"`
}

// Upstream describes the cascaded proxy all non-PAC traffic is forwarded to.
// URL must be http or https; basic auth is applied only when both Username
// and Password are non-empty.
type Upstream struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Geo governs the RU IPv4 delegation fetcher: where to fetch from, how often
// to refresh after success, and how often to retry after failure.
type Geo struct {
	SourceURL       string        `yaml:"source_url"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	RetryInterval   time.Duration `yaml:"retry_interval"`
}

// Routing holds the user-supplied white/black lists that override the RU geo
// rule. Entries match by suffix (*.example.com) or CIDR (10.0.0.0/8).
type Routing struct {
	Whitelist []string `yaml:"whitelist"`
	Blacklist []string `yaml:"blacklist"`
}

// PAC governs how the /proxy.pac HTTP response is served. Currently the only
// knob is the cache TTL, but the struct exists so future PAC-serving options
// (path override, content-type variants, etc.) have a home.
type PAC struct {
	// CacheSeconds controls the max-age of the served PAC, in seconds.
	//
	// This is a *int (not int) deliberately: most config fields use the
	// "zero value means default" convention, but for this field zero is a
	// meaningful operator choice (disable caching entirely → no-cache).
	// The pointer distinguishes:
	//   - nil  → field omitted in YAML → loader applies the default (15s)
	//   - &0   → operator explicitly wrote cache_seconds: 0 → no-cache
	//   - &N>0 → operator-set value, used as max-age
	// Use EffectiveCacheSeconds(default) to resolve to a concrete int.
	CacheSeconds *int `yaml:"cache_seconds"`
}

// DefaultPACCacheSeconds is the max-age applied when pac.cache_seconds is
// omitted. Chosen for a balance: short enough that hot-reloaded config
// reaches the browser within ~15s, long enough that the PAC isn't refetched
// on every single request.
const DefaultPACCacheSeconds = 15

// EffectiveCacheSeconds resolves the configured cache TTL to a concrete int.
// Returns defaultSec when CacheSeconds is nil (omitted in config); returns
// the pointed-to value otherwise (including 0, the explicit no-cache opt-in).
func (p PAC) EffectiveCacheSeconds(defaultSec int) int {
	if p.CacheSeconds == nil {
		return defaultSec
	}
	return *p.CacheSeconds
}

// Range is a single inclusive [Low, High] uint32 IPv4 numeric range, the
// canonical merged form used by the PAC's binary-search lookup.
type Range struct {
	Low  uint32
	High uint32
}

// RURanges is the set of disjoint sorted RU IPv4 ranges, derived from the
// RIPE delegation list and cached to disk.
type RURanges struct {
	Ranges    []Range
	UpdatedAt time.Time
}

// PACInput bundles the values the PAC renderer needs: the listen address
// (injected as PROXY target), the white/black lists, and the merged RU ranges.
type PACInput struct {
	Listen    string
	Whitelist []string
	Blacklist []string
	Ranges    RURanges
}

// Sentinel errors used across port boundaries. Wrap with %w when adding
// context; check with errors.Is at the consumer.
var (
	// ErrCacheMiss is returned when the geo cache file does not exist.
	ErrCacheMiss = errors.New("geo cache miss")
	// ErrCacheStale is returned when the cache exists but is past its
	// refresh_interval (a signal to perform a blocking startup fetch).
	ErrCacheStale = errors.New("geo cache stale")
	// ErrParseFailed is returned when a RIPE delegation payload cannot be
	// parsed into ranges.
	ErrParseFailed = errors.New("ripe parse failed")
	// ErrValidation is returned when a config value fails semantic
	// validation (bad URL scheme, non-positive duration, etc.).
	ErrValidation = errors.New("config validation failed")
)
