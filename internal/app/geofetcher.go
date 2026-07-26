package app

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// GeoFetcherDeps bundles everything the fetcher needs. Keeping it a struct
// makes the constructor readable and lets tests construct fakes easily.
type GeoFetcherDeps struct {
	Source    port.GeoSource
	Cache     port.Cache
	Clock     port.Clock
	SourceURL string
	CachePath string
	Refresh   time.Duration // interval after a successful refresh
	Retry     time.Duration // interval after a failed refresh

	RangesPtr *atomic.Pointer[port.RURanges] // publish merged ranges here
	Regen     chan<- struct{}                // signal PAC regen
	Errs      chan<- error                   // report non-fatal errors
}

// GeoFetcher runs the geo-data state machine: a blocking first-fetch at
// startup, then a ticker loop using the dual-interval schedule.
type GeoFetcher struct {
	deps GeoFetcherDeps
}

// NewGeoFetcher constructs a fetcher. Callers must Run(ctx) it.
func NewGeoFetcher(deps GeoFetcherDeps) *GeoFetcher {
	return &GeoFetcher{deps: deps}
}

// Run performs the startup fetch and then enters the ticker loop. It blocks
// until ctx is cancelled.
func (f *GeoFetcher) Run(ctx context.Context) {
	f.startup(ctx)
	f.loop(ctx)
}

// startup handles the blocking first-fetch logic with all three branches:
// fresh cache (skip), stale cache (fetch, fall back to stale on fail),
// missing cache (fetch, proceed empty on fail).
func (f *GeoFetcher) startup(ctx context.Context) {
	d := f.deps

	fresh, err := d.Cache.Freshness(d.CachePath, d.Refresh, d.Clock.Now())
	if err == nil && fresh {
		// Fresh cache: load and publish.
		ranges, err := d.Cache.Read(d.CachePath)
		if err != nil {
			// Freshness said present but Read failed — treat as miss.
			f.fetchAndPublish(ctx, true)
			return
		}
		f.publish(ranges)
		return
	}

	// Stale or missing: do the blocking fetch; fall back on failure.
	f.fetchAndPublish(ctx, err == nil) // stale == true when err==nil means present-but-stale
}

// fetchAndPublish performs a fetch (with ctx timeout) and publishes the result.
// On failure, if allowStale is true it tries to fall back to a stale cache;
// otherwise it publishes an empty range set (degraded mode).
func (f *GeoFetcher) fetchAndPublish(ctx context.Context, allowStale bool) {
	d := f.deps

	ranges, err := f.fetch(ctx)
	if err == nil {
		f.publish(ranges)
		return
	}

	// Fetch failed.
	f.reportErr(err)

	if allowStale {
		// Try the stale cache.
		if stale, errRead := d.Cache.Read(d.CachePath); errRead == nil {
			log.Printf("geo: fetch failed, serving stale cache (%d ranges)", len(stale.Ranges))
			f.publish(stale)
			return
		}
	}

	// No fallback available → degraded empty ranges.
	log.Printf("geo: fetch failed with no cache; serving empty ranges (all non-listed hosts route to upstream)")
	r := port.RURanges{UpdatedAt: d.Clock.Now()}
	f.publish(r)
}

// fetch retrieves the source, parses+merges, writes the cache, and returns the
// merged ranges. Any error aborts without publishing or caching partial data.
func (f *GeoFetcher) fetch(ctx context.Context) (port.RURanges, error) {
	d := f.deps
	body, err := d.Source.Fetch(ctx, d.SourceURL)
	if err != nil {
		return port.RURanges{}, err
	}
	merged, err := ParseAndMerge(string(body))
	if err != nil {
		return port.RURanges{}, err
	}
	ranges := port.RURanges{Ranges: merged, UpdatedAt: d.Clock.Now()}
	if err := d.Cache.Write(d.CachePath, ranges); err != nil {
		// Cache write failure is non-fatal; we still have the in-memory result.
		log.Printf("geo: cache write failed (non-fatal): %v", err)
	}
	return ranges, nil
}

// loop runs the dual-interval ticker: on each tick, fetch; choose the next
// interval based on success (Refresh) or failure (Retry).
func (f *GeoFetcher) loop(ctx context.Context) {
	d := f.deps
	interval := d.Refresh // assume success path initially; first fetch already happened

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			ranges, err := f.fetch(ctx)
			if err != nil {
				f.reportErr(err)
				interval = d.Retry
				continue
			}
			f.publish(ranges)
			interval = d.Refresh
		}
	}
}

// publish atomically stores ranges and signals PAC regeneration.
func (f *GeoFetcher) publish(ranges port.RURanges) {
	d := f.deps
	r := ranges
	d.RangesPtr.Store(&r)
	if d.Regen != nil {
		select {
		case d.Regen <- struct{}{}:
		default:
			// A render is already pending; coalesce.
		}
	}
}

// reportErr sends the error to the Errs channel non-blockingly; if the
// channel is full, it logs instead.
func (f *GeoFetcher) reportErr(err error) {
	d := f.deps
	if d.Errs == nil {
		return
	}
	select {
	case d.Errs <- err:
	default:
		log.Printf("geo: error channel full; dropping: %v", err)
	}
}
