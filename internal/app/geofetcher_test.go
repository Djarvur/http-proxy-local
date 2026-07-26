package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/app"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// fakeGeoSource is a controllable GeoSource for tests.
type fakeGeoSource struct {
	mu      sync.Mutex
	body    []byte
	failErr error
	calls   int
}

func (f *fakeGeoSource) Fetch(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failErr != nil {
		return nil, f.failErr
	}
	return f.body, nil
}

func (f *fakeGeoSource) setBody(b []byte) { f.mu.Lock(); f.body = b; f.mu.Unlock() }
func (f *fakeGeoSource) setFail(err error) { f.mu.Lock(); f.failErr = err; f.mu.Unlock() }
func (f *fakeGeoSource) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// memCache is an in-memory port.Cache for tests.
type memCache struct {
	data map[string]port.RURanges
}

func newMemCache() *memCache { return &memCache{data: map[string]port.RURanges{}} }

func (m *memCache) Read(path string) (port.RURanges, error) {
	if r, ok := m.data[path]; ok {
		return r, nil
	}
	return port.RURanges{}, port.ErrCacheMiss
}
func (m *memCache) Write(path string, ranges port.RURanges) error {
	if ranges.UpdatedAt.IsZero() {
		ranges.UpdatedAt = time.Now()
	}
	m.data[path] = ranges
	return nil
}
func (m *memCache) Freshness(path string, interval time.Duration, now time.Time) (bool, error) {
	r, ok := m.data[path]
	if !ok {
		return false, port.ErrCacheMiss
	}
	return now.Sub(r.UpdatedAt) < interval, nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

const ripeBody = "ripencc|RU|ipv4|1.2.3.0|256|x|x\nripencc|RU|ipv4|1.2.4.0|256|x|x\n"

func newFetcherFixture(t *testing.T) (
	src *fakeGeoSource,
	cache *memCache,
	clock *fakeClock,
	rangesPtr *atomic.Pointer[port.RURanges],
	regen chan struct{},
	errs chan error,
) {
	t.Helper()
	src = &fakeGeoSource{body: []byte(ripeBody)}
	cache = newMemCache()
	clock = &fakeClock{t: time.Unix(1700000000, 0).UTC()}
	rangesPtr = &atomic.Pointer[port.RURanges]{}
	regen = make(chan struct{}, 8)
	errs = make(chan error, 8)
	return
}

// drainOneRegen asserts a PAC regen signal arrives within a generous window.
func drainOneRegen(t *testing.T, ch chan struct{}) {
	t.Helper()
	const wait = 200 * time.Millisecond
	select {
	case <-ch:
	case <-time.After(wait):
		t.Fatal("expected a PAC regen signal, got none")
	}
}

func TestGeoFetcher_FreshCacheSkipsStartupFetch(t *testing.T) {
	t.Parallel()
	src, cache, clock, rangesPtr, regen, errs := newFetcherFixture(t)

	// Seed a fresh cache (written 1 hour ago, interval 24h → fresh).
	seed := port.RURanges{Ranges: []port.Range{{Low: 99, High: 100}}, UpdatedAt: clock.Now().Add(-time.Hour)}
	_ = cache.Write("cache", seed)

	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: "https://example/ripe",
		CachePath: "cache",
		Refresh:   24 * time.Hour,
		Retry:     5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Fresh cache → no fetch at startup.
	if got := src.callCount(); got != 0 {
		t.Fatalf("startup fetch happened despite fresh cache; calls=%d", got)
	}

	// Ranges must be published and PAC signaled.
	waitForRanges(t, rangesPtr, 99)
	drainOneRegen(t, regen)
}

func TestGeoFetcher_StaleCacheTriggersBlockingFetch(t *testing.T) {
	t.Parallel()
	src, cache, clock, rangesPtr, regen, errs := newFetcherFixture(t)

	// Seed a stale cache (48h old, interval 24h → stale) with old data.
	seed := port.RURanges{Ranges: []port.Range{{Low: 1, High: 2}}, UpdatedAt: clock.Now().Add(-48 * time.Hour)}
	_ = cache.Write("cache", seed)

	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: "https://example/ripe", CachePath: "cache",
		Refresh: 24 * time.Hour, Retry: 5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Stale → fetch happens; result reflects fetched body (1.2.3.0/24 + 1.2.4.0/24 merged).
	waitForRanges(t, rangesPtr, 16909056) // 1.2.3.0 as uint32
	if got := src.callCount(); got != 1 {
		t.Fatalf("expected 1 startup fetch, got %d", got)
	}
	drainOneRegen(t, regen)
}

func TestGeoFetcher_NoCacheTriggersBlockingFetch(t *testing.T) {
	t.Parallel()
	src, _, clock, rangesPtr, regen, errs := newFetcherFixture(t)

	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: newMemCache(), Clock: clock,
		SourceURL: "https://example/ripe", CachePath: "cache",
		Refresh: 24 * time.Hour, Retry: 5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitForRanges(t, rangesPtr, 16909056)
	drainOneRegen(t, regen)
}

func TestGeoFetcher_NoCacheFetchFails_ProceedsWithEmptyRanges(t *testing.T) {
	t.Parallel()
	src, _, clock, rangesPtr, regen, errs := newFetcherFixture(t)
	src.setFail(errors.New("network down"))

	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: newMemCache(), Clock: clock,
		SourceURL: "https://example/ripe", CachePath: "cache",
		Refresh:   24 * time.Hour,
		Retry:     5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Failure with no cache → empty ranges published (not nil, not crash).
	waitForRangesLen(t, rangesPtr, 0)

	// Error logged.
	select {
	case <-errs:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected an error for fetch failure, got none")
	}

	// PAC regen still signaled (with empty ranges).
	drainOneRegen(t, regen)
}

func TestGeoFetcher_StaleCacheFetchFails_FallsBackToStale(t *testing.T) {
	t.Parallel()
	src, cache, clock, rangesPtr, regen, errs := newFetcherFixture(t)
	stale := port.RURanges{Ranges: []port.Range{{Low: 42, High: 43}}, UpdatedAt: clock.Now().Add(-48 * time.Hour)}
	_ = cache.Write("cache", stale)
	src.setFail(errors.New("network down"))

	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: cache, Clock: clock,
		SourceURL: "https://example/ripe", CachePath: "cache",
		Refresh: 24 * time.Hour, Retry: 5 * time.Minute,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Fallback to stale → ranges reflect the cache (Low=42).
	waitForRanges(t, rangesPtr, 42)

	select {
	case <-errs:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected error for stale+fail")
	}
}

// tickerTest shortens intervals to ms-scale to make the dual-interval
// behavior testable in real time.
func TestGeoFetcher_Ticker_DualIntervalSchedule(t *testing.T) {
	t.Parallel()
	src, _, clock, rangesPtr, regen, errs := newFetcherFixture(t)

	// Short intervals: success→50ms, fail→20ms.
	f := app.NewGeoFetcher(app.GeoFetcherDeps{
		Source: src, Cache: newMemCache(), Clock: clock,
		SourceURL: "https://example/ripe", CachePath: "cache",
		Refresh: 50 * time.Millisecond, Retry: 20 * time.Millisecond,
		RangesPtr: rangesPtr, Regen: regen, Errs: errs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { f.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone }()

	// Initial blocking fetch (success).
	waitForRanges(t, rangesPtr, 16909056)
	firstCalls := src.callCount()

	// Wait past the success interval and confirm a second success fires.
	time.Sleep(80 * time.Millisecond)
	secondCalls := src.callCount()
	if secondCalls <= firstCalls {
		t.Errorf("success interval did not fire: calls %d→%d", firstCalls, secondCalls)
	}

	// Now fail and confirm retry interval (20ms) is faster than success (50ms).
	src.setFail(errors.New("transient"))
	preFailCalls := src.callCount()
	time.Sleep(120 * time.Millisecond)
	postFailCalls := src.callCount()
	// In 120ms with 20ms retry, expect at least 3 retries.
	if postFailCalls-preFailCalls < 3 {
		t.Errorf("retry interval too slow; expected ≥3 retries in 120ms, got %d", postFailCalls-preFailCalls)
	}

	// Recover and confirm schedule reverts to success interval.
	src.setBody([]byte(ripeBody))
	src.setFail(nil)
	preRecoverCalls := src.callCount()
	time.Sleep(120 * time.Millisecond)
	// 120ms / 50ms ≈ 2-3 success ticks; definitely fewer than 20ms retry.
	postRecoverCalls := src.callCount()
	if postRecoverCalls-preRecoverCalls > 5 {
		t.Errorf("after recovery, schedule didn't slow to refresh interval; calls=%d in 120ms", postRecoverCalls-preRecoverCalls)
	}
}

func waitForRanges(t *testing.T, p *atomic.Pointer[port.RURanges], wantLow uint32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r := p.Load(); r != nil {
			for _, rng := range r.Ranges {
				if rng.Low == wantLow {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("ranges never contained Low=%d", wantLow)
}

func waitForRangesLen(t *testing.T, p *atomic.Pointer[port.RURanges], want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r := p.Load(); r != nil && len(r.Ranges) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	r := p.Load()
	if r == nil {
		t.Fatalf("ranges never published; want len=%d", want)
	}
	t.Fatalf("ranges len never reached %d; got %d", want, len(r.Ranges))
}
