package fscache_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/out/fscache"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

func TestWriteRead_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ru.cache")

	c := fscache.New()
	in := port.RURanges{
		Ranges:    []port.Range{{Low: 10, High: 20}, {Low: 30, High: 40}},
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}

	if err := c.Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// File exists at the target path.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not at target path: %v", err)
	}
	// No leftover temp file.
	matches, _ := filepath.Glob(path + ".*")
	if len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}

	got, err := c.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Ranges) != 2 {
		t.Fatalf("Ranges len = %d, want 2", len(got.Ranges))
	}
	if got.Ranges[0] != (port.Range{Low: 10, High: 20}) {
		t.Errorf("Range[0] = %+v", got.Ranges[0])
	}
	if !got.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, in.UpdatedAt)
	}
}

func TestRead_MissingFile(t *testing.T) {
	t.Parallel()
	c := fscache.New()
	_, err := c.Read(filepath.Join(t.TempDir(), "nope.cache"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestWrite_Atomic_NoCorruptionOnSimulatedCrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ru.cache")

	// Pre-existing good cache.
	c := fscache.New()
	good := port.RURanges{Ranges: []port.Range{{Low: 1, High: 2}}, UpdatedAt: time.Unix(1, 0)}
	if err := c.Write(path, good); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	// Read it back to confirm we have a known-good baseline.
	if _, err := c.Read(path); err != nil {
		t.Fatalf("baseline Read: %v", err)
	}

	// The atomic write contract: a temp file is written, then renamed. If
	// we simulate a crash by leaving a stale .tmp file in the dir, the
	// previously-written good cache must remain readable and intact.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("partial junk"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}

	got, err := c.Read(path)
	if err != nil {
		t.Fatalf("Read after simulated crash: %v", err)
	}
	if len(got.Ranges) != 1 || got.Ranges[0] != (port.Range{Low: 1, High: 2}) {
		t.Errorf("cache corrupted by stale tmp: %+v", got)
	}
}

func TestFreshness(t *testing.T) {
	t.Parallel()
	c := fscache.New()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0).UTC()
	interval := 24 * time.Hour

	t.Run("missing file returns error", func(t *testing.T) {
		t.Parallel()
		_, err := c.Freshness(filepath.Join(dir, "nope.cache"), interval, now)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("fresh when within interval", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(dir, "fresh.cache")
		if err := c.Write(p, port.RURanges{UpdatedAt: now.Add(-1 * time.Hour)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		fresh, err := c.Freshness(p, interval, now)
		if err != nil {
			t.Fatalf("Freshness: %v", err)
		}
		if !fresh {
			t.Error("expected fresh, got stale")
		}
	})

	t.Run("stale when past interval", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(dir, "stale.cache")
		if err := c.Write(p, port.RURanges{UpdatedAt: now.Add(-48 * time.Hour)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		fresh, err := c.Freshness(p, interval, now)
		if err != nil {
			t.Fatalf("Freshness: %v", err)
		}
		if fresh {
			t.Error("expected stale, got fresh")
		}
	})

	t.Run("fresh at exact boundary minus epsilon", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(dir, "boundary.cache")
		// UpdatedAt = now - interval + 1s → still fresh.
		if err := c.Write(p, port.RURanges{UpdatedAt: now.Add(-interval + time.Second)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		fresh, err := c.Freshness(p, interval, now)
		if err != nil {
			t.Fatalf("Freshness: %v", err)
		}
		if !fresh {
			t.Error("expected fresh at boundary-epsilon, got stale")
		}
	})
}
