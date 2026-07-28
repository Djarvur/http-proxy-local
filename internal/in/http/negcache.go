// Package httpserver is the inbound HTTP adapter.
package httpserver

import (
	"sync"
	"time"
)

// negativeCache records hostnames whose Direct-dial strategy has recently
// failed, so subsequent requests skip the doomed Direct attempt and go
// straight to the next strategy (typically Upstream). Keyed by hostname
// (host-level unavailability, not per-port).
//
// The cache is consulted ONLY on the Direct path of the blacklist fallback
// (the only rule with a fallback). RU-geo Direct has no fallback and does not
// consult it; Upstream strategies never consult it.
//
// Eviction is lazy: expired entries remain in the map but are reported as
// not-negative. The working set is tiny (PAC-blind clients talk to a handful
// of hosts), so unbounded growth is acceptable for v1 — a future change can
// add a background sweeper if it ever matters.
type negativeCache struct {
	mu   sync.RWMutex
	host map[string]time.Time // hostname → expiry
	now  func() time.Time
}

// NewNegativeCache returns a negative cache using the real clock. It is the
// constructor used by the composition root (wire.go) for the smart listener.
func NewNegativeCache() *negativeCache {
	return newNegativeClock(time.Now)
}

// newNegativeCache returns a negative cache using the real clock (unexported
// alias for in-package tests).
func newNegativeCache() *negativeCache {
	return newNegativeClock(time.Now)
}

// newNegativeClock returns a negative cache whose clock is the given function.
// Tests inject a controllable clock so TTL expiry is observable without
// sleeping.
func newNegativeClock(now func() time.Time) *negativeCache {
	return &negativeCache{
		host: make(map[string]time.Time),
		now:  now,
	}
}

// IsNegative reports whether host was marked negative and the TTL has not yet
// elapsed. Read-locks; safe for concurrent use.
func (n *negativeCache) IsNegative(host string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	expiry, ok := n.host[host]
	if !ok {
		return false
	}
	return n.now().Before(expiry)
}

// MarkNegative records that host has failed a Direct dial, with the given TTL.
// Write-locks; safe for concurrent use. Re-marking extends the expiry.
func (n *negativeCache) MarkNegative(host string, ttl time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.host[host] = n.now().Add(ttl)
}
