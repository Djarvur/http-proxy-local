package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// ParseRIPELine parses one line of the RIPE delegation format
// (registry|country|type|start|value|date|status[|...]).
//
// Returns:
//   - (Range, true, nil) for a RU ipv4 record.
//   - (zero, false, nil) for non-RU, non-ipv4, summary, or comment lines
//     (legitimately skipped — not an error).
//   - (zero, false, err) for a line that looks like a RU ipv4 record but is
//     malformed (bad IP, non-numeric count, wrong field count).
func ParseRIPELine(line string) (port.Range, bool, error) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return port.Range{}, false, nil
	}

	fields := strings.Split(s, "|")

	// A line that starts with <registry>|RU|ipv4| but is truncated (< 7
	// fields) is a clearly-broken RU record → error, not silent skip. Check
	// this BEFORE the generic short-line skip below.
	if len(fields) >= 4 && fields[1] == "RU" && fields[2] == "ipv4" && len(fields) < 7 {
		return port.Range{}, false, fmt.Errorf("%w: truncated RU ipv4 line %q", port.ErrParseFailed, s)
	}

	// Lines shorter than 7 fields that aren't RU-ipv4 are summary fragments,
	// comments, or garbage — skip silently.
	if len(fields) < 7 {
		return port.Range{}, false, nil
	}

	// Summary records begin with a version integer (e.g. "2|ripencc|...").
	registry := fields[0]
	if _, err := strconv.Atoi(registry); err == nil {
		// First field numeric → summary/version line, skip.
		return port.Range{}, false, nil
	}

	country := fields[1]
	typ := fields[2]
	if country != "RU" || typ != "ipv4" {
		return port.Range{}, false, nil
	}

	start := fields[3]
	countStr := fields[4]

	ip := net.ParseIP(start)
	if ip == nil {
		return port.Range{}, false, fmt.Errorf("%w: bad ip %q", port.ErrParseFailed, start)
	}
	// net.ParseIP accepts IPv6 forms too; require a 4-byte IPv4.
	ip4 := ip.To4()
	if ip4 == nil {
		return port.Range{}, false, fmt.Errorf("%w: not ipv4 %q", port.ErrParseFailed, start)
	}

	count, err := strconv.ParseUint(countStr, 10, 32)
	if err != nil {
		return port.Range{}, false, fmt.Errorf("%w: bad count %q: %w", port.ErrParseFailed, countStr, err)
	}
	if count == 0 {
		return port.Range{}, false, fmt.Errorf("%w: zero count", port.ErrParseFailed)
	}

	low := ipToUint32(ip4)
	high := low + uint32(count) - 1
	return port.Range{Low: low, High: high}, true, nil
}

// MergeRanges sorts the input by Low and merges adjacent (Low == prev.High+1)
// or overlapping ranges into the minimal disjoint set. Returns a new slice;
// the input is not mutated beyond what sort.Slice does on its backing array
// (callers pass ownership; MergeRanges makes its own sorted copy).
func MergeRanges(in []port.Range) []port.Range {
	if len(in) == 0 {
		return nil
	}
	sorted := make([]port.Range, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Low != sorted[j].Low {
			return sorted[i].Low < sorted[j].Low
		}
		return sorted[i].High < sorted[j].High
	})

	merged := make([]port.Range, 0, len(sorted))
	cur := sorted[0]
	for _, r := range sorted[1:] {
		if r.Low <= cur.High+1 { // adjacent or overlapping
			if r.High > cur.High {
				cur.High = r.High
			}
			continue
		}
		merged = append(merged, cur)
		cur = r
	}
	merged = append(merged, cur)
	return merged
}

// ParseAndMerge end-to-end: read raw RIPE delegation text, parse each line,
// collect RU ipv4 ranges, and return the merged disjoint set.
//
// An unparseable line that looks like a RU ipv4 record returns an error and
// NO partial output (the caller must not cache a half-built range set).
func ParseAndMerge(raw string) ([]port.Range, error) {
	var ranges []port.Range
	scanner := bufio.NewScanner(bytes.NewReader([]byte(raw)))
	// RIPE lines can be long but well under 64KB; raise to be safe.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		r, ok, err := ParseRIPELine(scanner.Text())
		if err != nil {
			return nil, err
		}
		if ok {
			ranges = append(ranges, r)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scan error: %w", port.ErrParseFailed, err)
	}
	return MergeRanges(ranges), nil
}

// ipToUint32 converts a 4-byte IPv4 to its uint32 numeric form. ip must be a
// To4() result (length 4); caller guarantees.
func ipToUint32(ip net.IP) uint32 {
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// Compile-time guard: ErrParseFailed must satisfy errors.Is (it's a sentinel).
var _ = errors.Is
