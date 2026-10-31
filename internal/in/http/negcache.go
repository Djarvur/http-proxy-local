// Package httpserver is the inbound HTTP adapter.
package httpserver

import (
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/dialchain"
)

// negativeCache is the shared dialchain negative cache. The implementation
// used to live here; it moved to internal/in/dialchain so the transparent
// listener shares it (change add-transparent-listener, design D1). The type
// alias + constructors keep the smart listener's tests and call sites
// unchanged.
type negativeCache = dialchain.NegativeCache

// NewNegativeCache returns a negative cache using the real clock. It is the
// constructor used by the composition root (wire.go) for the smart listener.
func NewNegativeCache() *negativeCache {
	return dialchain.NewNegativeCache()
}

// newNegativeCache returns a negative cache using the real clock (unexported
// alias for in-package tests).
func newNegativeCache() *negativeCache {
	return dialchain.NewNegativeCache()
}

// newNegativeClock returns a negative cache whose clock is the given function.
// Tests inject a controllable clock so TTL expiry is observable without
// sleeping.
func newNegativeClock(now func() time.Time) *negativeCache {
	return dialchain.NewNegativeCacheAt(now)
}
