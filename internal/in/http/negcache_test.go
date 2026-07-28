package httpserver

import (
	"sync"
	"testing"
	"time"
)

// TestNegativeCache exercises the hostname-keyed negative cache that suppresses
// repeated Direct-dial failures. The cache uses an injected now() function so
// TTL expiry is testable without real sleeping.
func TestNegativeCache(t *testing.T) {
	t.Parallel()

	t.Run("unseen host is not negative", func(t *testing.T) {
		t.Parallel()
		nc := newNegativeCache()
		if nc.IsNegative("z.ai") {
			t.Errorf("IsNegative(unseen) = true, want false")
		}
	})

	t.Run("marked host is negative within TTL", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		nc := newNegativeClock(func() time.Time { return now })
		nc.MarkNegative("z.ai", 60*time.Second)
		if !nc.IsNegative("z.ai") {
			t.Errorf("IsNegative after MarkNegative = false, want true")
		}
	})

	t.Run("host stops being negative after TTL", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		nc := newNegativeClock(func() time.Time { return now })
		nc.MarkNegative("z.ai", 60*time.Second)
		// Advance past the TTL.
		now = now.Add(61 * time.Second)
		if nc.IsNegative("z.ai") {
			t.Errorf("IsNegative after TTL expired = true, want false")
		}
	})

	t.Run("boundary: exactly at expiry is no longer negative", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		nc := newNegativeClock(func() time.Time { return now })
		nc.MarkNegative("z.ai", 60*time.Second)
		// time.Before is exclusive: the entry is valid on [mark, mark+60s) and
		// expires exactly at mark+60s. One nanosecond before expiry it is still
		// negative; at expiry and after it is not.
		now = now.Add(60*time.Second - 1*time.Nanosecond)
		if !nc.IsNegative("z.ai") {
			t.Errorf("IsNegative 1ns before expiry = false, want true")
		}
		now = now.Add(1 * time.Nanosecond) // now exactly at expiry
		if nc.IsNegative("z.ai") {
			t.Errorf("IsNegative exactly at expiry = true, want false (Before is exclusive)")
		}
	})

	t.Run("MarkNegative extends expiry", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		nc := newNegativeClock(func() time.Time { return now })
		nc.MarkNegative("z.ai", 60*time.Second)
		// Re-mark later with a fresh TTL.
		now = now.Add(30 * time.Second)
		nc.MarkNegative("z.ai", 60*time.Second)
		// 40s later — the first TTL would have expired (30+40=70 > 60), but the
		// second (set at +30, expires at +90) is still live.
		now = now.Add(40 * time.Second)
		if !nc.IsNegative("z.ai") {
			t.Errorf("IsNegative after re-mark within extended TTL = false, want true")
		}
	})

	t.Run("entries are host-scoped, not port-scoped", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		nc := newNegativeClock(func() time.Time { return now })
		nc.MarkNegative("z.ai", 60*time.Second)
		// Different host unaffected.
		if nc.IsNegative("other.example.com") {
			t.Errorf("IsNegative(other host) = true, want false")
		}
	})

	t.Run("concurrent MarkNegative and IsNegative do not race", func(t *testing.T) {
		t.Parallel()
		// This test is meaningful under `go test -race`.
		nc := newNegativeCache()
		const host = "concurrent.example.com"
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				nc.MarkNegative(host, time.Second)
			}()
			go func() {
				defer wg.Done()
				_ = nc.IsNegative(host)
			}()
		}
		wg.Wait()
	})
}
