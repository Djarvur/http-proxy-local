package port

import (
	"context"
	"time"
)

// GeoSource is the Out port for fetching raw geo-data payloads. The ripe
// adapter implements it; tests substitute fakes.
type GeoSource interface {
	// Fetch retrieves the raw delegation text from the given URL. A non-2xx
	// status, network error, or context cancellation yields an error.
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Cache is the Out port for persisting merged RU ranges to disk. The fscache
// adapter implements it.
type Cache interface {
	// Read loads merged ranges previously written. Returns ErrCacheMiss if
	// the file does not exist.
	Read(path string) (RURanges, error)
	// Write atomically persists the ranges (temp-then-rename). Updates
	// UpdatedAt to the write time.
	Write(path string, ranges RURanges) error
	// Freshness reports whether the cache at path is fresh relative to the
	// given interval and "now" timestamp. Returns ErrCacheMiss if absent.
	Freshness(path string, interval time.Duration, now time.Time) (bool, error)
}

// Clock is the Out port for reading the current time. Tests substitute fakes
// to control freshness decisions deterministically.
type Clock interface {
	Now() time.Time
}

// ConfigLoader is the Out port for parsing + validating config.yaml. The
// yamlconfig adapter implements it.
type ConfigLoader interface {
	// Load reads, parses, applies defaults, and returns a typed Config.
	Load(path string) (Config, error)
	// Validate checks semantic constraints (URL scheme, positive durations,
	// host:port). Returns ErrValidation-wrapped error on failure.
	Validate(c Config) error
}
