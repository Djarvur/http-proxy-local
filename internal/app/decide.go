package app

import (
	"net"
	"strings"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// StrategyPath selects which dial path a strategy uses.
type StrategyPath int

const (
	// StrategyDirect dials the target directly (no upstream).
	StrategyDirect StrategyPath = iota
	// StrategyUpstream dials the target via the configured upstream cascade.
	StrategyUpstream
)

// Strategy is a single dial attempt in the ordered list produced by Decide.
//
//   - Path == StrategyDirect: dial the target directly. DialIP, when non-nil,
//     pins the dial to that specific IP (used by the RU-geo rule, which matches
//     a specific resolved IP). When nil, dial by hostname.
//   - Path == StrategyUpstream: dial via cascade.Dial with the configured
//     upstream. DialIP is ignored.
type Strategy struct {
	Path   StrategyPath
	DialIP net.IP // Direct only; nil = dial by hostname
}

// Decide evaluates the routing precedence for host (with resolved ips) and
// returns an ordered list of dial strategies. The precedence mirrors the PAC's
// FindProxyForURL, first match wins:
//
//  1. blacklist → [Direct, Upstream] (direct first, upstream fallback)
//  2. whitelist → [Upstream]
//  3. plain hostname / private network → [Direct]
//  4. RU IPv4 (any resolved IP in a RU range) → [Direct], pinned to the first
//     matching RU IP
//  5. default → [Upstream]
//
// Pure function: no I/O. Callers supply the resolved ips (the smart listener
// resolves via the system resolver before calling Decide when an IP is needed).
// An empty ips slice is valid — suffix and plain-hostname rules match without
// IPs, and the geo rule simply does not match.
//
// Parity with the PAC is enforced by decide_parity_test.go.
func Decide(host string, ips []net.IP, cfg port.Config, ranges port.RURanges) []Strategy {
	host = strings.ToLower(host)

	// 1. blacklist — absolute veto over whitelist/local/geo, but the smart
	//    listener falls back to upstream if direct fails.
	if matchBlacklist(host, ips, cfg) {
		return []Strategy{{Path: StrategyDirect}, {Path: StrategyUpstream}}
	}

	// 2. whitelist — upstream only, no fallback.
	if matchSuffix(host, cfg.Routing.Whitelist) || matchCIDR(ips, cfg.Routing.Whitelist) {
		return []Strategy{{Path: StrategyUpstream}}
	}

	// 3. local/private network — direct only.
	if isPlain(host) || isPrivate(ips) {
		return []Strategy{{Path: StrategyDirect}}
	}

	// 4. RU IPv4 — direct, pinned to the first RU IP. No fallback (the operator
	//    moves the host to the whitelist if direct is unreliable).
	if ip, ok := firstRU(ips, ranges); ok {
		return []Strategy{{Path: StrategyDirect, DialIP: ip}}
	}

	// 5. default.
	return []Strategy{{Path: StrategyUpstream}}
}

// matchBlacklist reports whether host (by suffix) or ips (by CIDR) match the
// blacklist. Suffix and CIDR entries are not partitioned ahead of time — a
// pattern containing a "/" that parses as IPv4-CIDR is treated as CIDR, else
// suffix. This mirrors splitMatchers in pac.go.
func matchBlacklist(host string, ips []net.IP, cfg port.Config) bool {
	return matchSuffix(host, cfg.Routing.Blacklist) || matchCIDR(ips, cfg.Routing.Blacklist)
}

// matchSuffix reports whether host matches any suffix pattern via shExpMatch
// semantics. A pattern "*.example.com" matches "example.com" and any subdomain;
// a bare pattern matches the host verbatim. Matching is case-insensitive (host
// is already lowercased by Decide).
func matchSuffix(host string, patterns []string) bool {
	for _, p := range patterns {
		if isIPv4CIDR(p) {
			continue // CIDR patterns are handled by matchCIDR
		}
		if shExpMatch(host, p) {
			return true
		}
	}
	return false
}

// matchCIDR reports whether any IP in ips falls in any CIDR-pattern among
// patterns. Only patterns that parse as IPv4 CIDRs are considered; others are
// ignored (they are suffix patterns, handled by matchSuffix).
func matchCIDR(ips []net.IP, patterns []string) bool {
	if len(ips) == 0 {
		return false
	}
	for _, p := range patterns {
		if !isIPv4CIDR(p) {
			continue
		}
		_, ipNet, err := net.ParseCIDR(p)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if ip.To4() != nil && ipNet.Contains(ip.To4()) {
				return true
			}
		}
	}
	return false
}

// shExpMatch mirrors the PAC's shExpMatch: glob matching where "*" matches any
// sequence (including empty). PAC convention: "*.example.com" matches both
// "example.com" and "anything.example.com".
func shExpMatch(host, pattern string) bool {
	// Lowercase the pattern to match the already-lowercased host.
	pattern = strings.ToLower(pattern)
	// Optimize the common "*.suffix" case: match if host == suffix or host ends
	// with ".suffix".
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[2:]
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	}
	if pattern == "*" {
		return true
	}
	return host == pattern
}

// isPlain reports whether host is a plain hostname (no dots), mirroring the
// PAC's isPlainHostName. "localhost", "intranet", "mail" are plain; anything
// containing a dot is not. Used to short-circuit to Direct for names that are
// inherently local.
func isPlain(host string) bool {
	return !strings.Contains(host, ".")
}

// isPrivate reports whether any IP in ips is in an RFC1918/loopback/link-local
// range. Mirrors the PAC's isLocal IP checks.
func isPrivate(ips []net.IP) bool {
	for _, raw := range ips {
		ip := raw.To4()
		if ip == nil {
			continue // IPv6 private ranges are out of scope (parity with PAC)
		}
		switch {
		case ip[0] == 10:
			return true
		case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
			return true
		case ip[0] == 192 && ip[1] == 168:
			return true
		case ip[0] == 127:
			return true
		case ip[0] == 169 && ip[1] == 254:
			return true
		}
	}
	return false
}

// firstRU returns the first IPv4 in ips that falls in any RU range, scanning
// ips in order. The binary search mirrors the PAC's inRu function over the same
// sorted disjoint port.RURanges. Returns (ip, false) if no RU IP is found or
// ranges is empty.
func firstRU(ips []net.IP, ranges port.RURanges) (net.IP, bool) {
	if len(ranges.Ranges) == 0 {
		return nil, false
	}
	for _, raw := range ips {
		ip := raw.To4()
		if ip == nil {
			continue // IPv6 geo is out of scope (parity with PAC)
		}
		n := ipToUint32(ip)
		if inRanges(n, ranges.Ranges) {
			// Return the original (non-To4) form so callers see the canonical
			// representation; net.ParseIP already gives 16-byte form, and
			// dialing accepts either. Use a clean 4-byte form for dialing.
			return net.IPv4(ip[0], ip[1], ip[2], ip[3]), true
		}
	}
	return nil, false
}

// inRanges reports whether n falls in any [Low, High] pair of the sorted
// disjoint ranges, via binary search. Identical algorithm to the PAC's inRu.
func inRanges(n uint32, ranges []port.Range) bool {
	lo, hi := 0, len(ranges)-1
	for lo <= hi {
		m := (lo + hi) >> 1
		r := ranges[m]
		switch {
		case n < r.Low:
			hi = m - 1
		case n > r.High:
			lo = m + 1
		default:
			return true
		}
	}
	return false
}
