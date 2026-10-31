// Package dialchain holds the shared strategy-walking dial chain used by the
// smart HTTP listener and the transparent listener: resolve (only when a rule
// needs an IP), app.Decide for the ordered strategy list, Direct attempts
// bounded by timeout and count, negative-cache consult/record, and upstream
// cascade dials. Extracted from internal/in/http (change
// add-transparent-listener, design D1) so the transparent adapter reuses the
// identical behavior instead of duplicating it.
package dialchain

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// ResolveFunc resolves a hostname to its IPs. Real usage injects an adaptor
// over net.Resolver.LookupIPAddr; tests inject fakes.
type ResolveFunc func(ctx context.Context, host string) ([]net.IP, error)

// DirectDialFunc dials a Direct target (by hostname or by a pinned IP). It is
// the seam through which the Direct path is tested: real usage hands in
// net.Dialer.DialContext; tests hand in spies.
type DirectDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// UpstreamDialFunc dials the configured upstream cascade to a target
// host:port. Real usage hands in a closure that captures the live
// port.Upstream and calls cascade.Dial; tests hand in spies.
type UpstreamDialFunc func(target string) (net.Conn, error)

// NegativeCache records hostnames whose Direct-dial strategy has recently
// failed, so subsequent dials skip the doomed Direct attempt and go straight
// to the next strategy (typically Upstream). Keyed by hostname (host-level
// unavailability, not per-port).
//
// The cache is consulted ONLY on the Direct path of a fallback-capable
// strategy list. RU-geo Direct has no fallback and neither consults nor
// populates it; Upstream strategies never touch it.
//
// Eviction is lazy: expired entries remain in the map but are reported as
// not-negative. The working set is tiny, so unbounded growth is acceptable.
type NegativeCache struct {
	mu   sync.RWMutex
	host map[string]time.Time // hostname → expiry
	now  func() time.Time
}

// NewNegativeCache returns a negative cache using the real clock. It is the
// constructor used by the composition root (wire.go).
func NewNegativeCache() *NegativeCache {
	return &NegativeCache{host: make(map[string]time.Time), now: time.Now}
}

// NewNegativeCacheAt returns a negative cache whose clock is the given
// function. Tests inject a controllable clock so TTL expiry is observable
// without sleeping.
func NewNegativeCacheAt(now func() time.Time) *NegativeCache {
	return &NegativeCache{host: make(map[string]time.Time), now: now}
}

// IsNegative reports whether host was marked negative and the TTL has not yet
// elapsed. Read-locks; safe for concurrent use.
func (n *NegativeCache) IsNegative(host string) bool {
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
func (n *NegativeCache) MarkNegative(host string, ttl time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.host[host] = n.now().Add(ttl)
}

// Chain walks the ordered strategy list produced by app.Decide for a target
// and returns the first successfully established connection. The zero value
// is not usable; construct with New.
type Chain struct {
	resolve  ResolveFunc
	direct   DirectDialFunc
	upstream UpstreamDialFunc
	negCache *NegativeCache
}

// New assembles a Chain from its seams.
func New(resolve ResolveFunc, direct DirectDialFunc, upstream UpstreamDialFunc, negCache *NegativeCache) *Chain {
	return &Chain{resolve: resolve, direct: direct, upstream: upstream, negCache: negCache}
}

// Dial resolves the target host (only if a later rule needs an IP), evaluates
// the precedence via app.Decide, and walks the resulting strategy list in
// order until one establishes a connection.
//
// Direct strategy semantics:
//   - If the host is in the negative cache (and this is the fallback-capable
//     path), skip the Direct attempt and move to the next strategy.
//   - Otherwise dial up to cfg.Smart.DialAttempts times, each bounded by
//     cfg.Smart.DialTimeout. On all-attempts failure, mark the host negative
//     (TTL from cfg.Smart.NegativeCacheTTL) and continue.
//   - For the RU-geo rule, dial the pinned DialIP; for other Direct rules,
//     dial by hostname. RU failures do NOT mark the negative cache (there is
//     no fallback to skip).
//
// Upstream strategy semantics: dial via the upstream closure once. Upstream
// failures never consult or update the negative cache.
//
// The tuning fields are read from cfg.Smart — the transparent listener uses
// the same block so a single tuning configuration governs both listeners
// (change add-transparent-listener, design D1).
func (c *Chain) Dial(ctx context.Context, target string, cfg port.Config, ranges port.RURanges) (net.Conn, error) {
	host, portNum, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("dialchain split host:port: %w", err)
	}

	ips, err := c.resolve(ctx, host)
	if err != nil {
		// Resolution failure is non-fatal: Decide handles nil ips by skipping
		// CIDR/geo rules and falling through to suffix/plain/default.
		ips = nil
	}

	strategies := app.Decide(host, ips, cfg, ranges)
	for _, s := range strategies {
		switch s.Path {
		case app.StrategyDirect:
			if c.negCache.IsNegative(host) && hasUpstreamFallback(strategies) {
				continue // skip doomed Direct, try the fallback
			}
			conn, derr := dialDirect(ctx, s, host, portNum, cfg.Smart.DialTimeout, cfg.Smart.DialAttempts, c.direct)
			if derr == nil {
				return conn, nil
			}
			// Only mark negative when there is a downstream fallback to skip.
			// RU-geo Direct (no fallback) must not pollute the cache.
			if hasUpstreamFallback(strategies) && cfg.Smart.NegativeCacheTTL > 0 {
				c.negCache.MarkNegative(host, cfg.Smart.NegativeCacheTTL)
			}
		case app.StrategyUpstream:
			conn, uerr := c.upstream(target)
			if uerr == nil {
				return conn, nil
			}
		}
	}
	return nil, fmt.Errorf("all strategies failed for %s", target)
}

// dialDirect attempts up to attempts Direct dials, each bounded by timeout.
// The dial target is "<DialIP>:<port>" when the strategy carries a pinned IP
// (RU-geo), else "<host>:<port>". Returns the first successful conn.
func dialDirect(
	ctx context.Context,
	s app.Strategy,
	host, portNum string,
	timeout time.Duration,
	attempts int,
	direct DirectDialFunc,
) (net.Conn, error) {
	if attempts < 1 {
		attempts = 1
	}
	target := host
	if s.DialIP != nil {
		target = s.DialIP.String()
	}
	addr := net.JoinHostPort(target, portNum)
	var lastErr error
	for i := 0; i < attempts; i++ {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := direct(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// hasUpstreamFallback reports whether the strategy list contains an Upstream
// entry after a Direct entry — i.e. whether a Direct failure has somewhere to
// fall back to. Used to decide whether to consult/populate the negative cache.
func hasUpstreamFallback(strategies []app.Strategy) bool {
	seenDirect := false
	for _, s := range strategies {
		if s.Path == app.StrategyDirect {
			seenDirect = true
		}
		if seenDirect && s.Path == app.StrategyUpstream {
			return true
		}
	}
	return false
}
