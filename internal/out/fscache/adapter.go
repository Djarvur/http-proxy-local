// Package fscache implements port.Cache: it persists merged RU ranges to a
// disk file with atomic write semantics (temp-then-rename) so a crash
// mid-write cannot corrupt the previously-good cache.
//
// File format (line-oriented, debuggable):
//
//	# updated <unix-seconds>
//	<low> <high>
//	<low> <high>
//	...
//
// The first line is the freshness timestamp; subsequent lines are decimal
// uint32 pairs separated by a single space.
package fscache

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// Cache implements port.Cache against the local filesystem.
type Cache struct{}

// New returns a Cache.
func New() Cache { return Cache{} }

// Write persists ranges atomically: it serializes to a temp file in the same
// directory, fsyncs, then renames over the target path. UpdatedAt is set to
// time.Now() unless the caller already set it (caller wins, so tests can
// pin timestamps).
func (Cache) Write(path string, ranges port.RURanges) error {
	if ranges.UpdatedAt.IsZero() {
		ranges.UpdatedAt = time.Now().UTC()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fscache: mkdir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("fscache: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if any step below fails before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := writeCache(tmp, ranges); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fscache: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fscache: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("fscache: rename temp→%q: %w", path, err)
	}
	return nil
}

// writeCache serializes ranges to w in the documented text format.
func writeCache(w io.Writer, ranges port.RURanges) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "# updated %d\n", ranges.UpdatedAt.Unix()); err != nil {
		return fmt.Errorf("fscache: write header: %w", err)
	}
	for _, r := range ranges.Ranges {
		if _, err := fmt.Fprintf(bw, "%d %d\n", r.Low, r.High); err != nil {
			return fmt.Errorf("fscache: write range: %w", err)
		}
	}
	return bw.Flush()
}

// Read loads ranges previously written by Write. Returns port.ErrCacheMiss
// (wrapped) when the file does not exist.
func (Cache) Read(path string) (port.RURanges, error) {
	f, err := os.Open(path) //nolint:gosec // G304: cache path is operator/config-derived
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return port.RURanges{}, fmt.Errorf("%w: %s", port.ErrCacheMiss, path)
		}
		return port.RURanges{}, fmt.Errorf("fscache: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only; close error is not actionable

	var out port.RURanges
	scanner := bufio.NewScanner(f)
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			first = false
			continue
		}
		if strings.HasPrefix(line, "#") {
			if first {
				out.UpdatedAt = parseHeaderTimestamp(line)
			}
			first = false
			continue
		}
		first = false

		r, err := parseRangeLine(line)
		if err != nil {
			return port.RURanges{}, err
		}
		out.Ranges = append(out.Ranges, r)
	}
	if err := scanner.Err(); err != nil {
		return port.RURanges{}, fmt.Errorf("fscache: scan %q: %w", path, err)
	}
	return out, nil
}

// parseHeaderTimestamp extracts the Unix timestamp from a "# updated <ts>"
// header line. Returns zero time on any parse failure (the caller treats
// zero as "unknown age → stale").
func parseHeaderTimestamp(line string) time.Time {
	const prefix = "# updated "
	if !strings.HasPrefix(line, prefix) {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

// parseRangeLine parses a single "<low> <high>" line into a Range.
func parseRangeLine(line string) (port.Range, error) {
	parts := strings.Fields(line)
	if len(parts) != 2 {
		return port.Range{}, fmt.Errorf("%w: bad line %q", port.ErrParseFailed, line)
	}
	lo, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return port.Range{}, fmt.Errorf("%w: bad low %q", port.ErrParseFailed, parts[0])
	}
	hi, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return port.Range{}, fmt.Errorf("%w: bad high %q", port.ErrParseFailed, parts[1])
	}
	return port.Range{Low: uint32(lo), High: uint32(hi)}, nil
}

// Freshness reports whether the cache at path is fresh: it must exist, parse,
// and have UpdatedAt within `interval` of `now`. A missing file returns an
// error (callers should treat that as "not fresh, fetch needed").
func (Cache) Freshness(path string, interval time.Duration, now time.Time) (bool, error) {
	r, err := Cache{}.Read(path)
	if err != nil {
		return false, err
	}
	age := now.Sub(r.UpdatedAt)
	return age < interval, nil
}
