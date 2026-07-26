package app

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// pacTemplate is the PAC body. It is deliberately ES3-subset JavaScript:
// only `var`/`function`, no arrow functions, no template literals, no
// `const`/`let`. PAC engines across browsers are restricted to this subset.
//
// Precedence (evaluated top-down in FindProxyForURL, first match wins):
//  1. blacklist  → "DIRECT"   (absolute; overrides whitelist, local, geo)
//  2. whitelist  → "PROXY"    (overrides local, geo)
//  3. local/private network → "DIRECT"
//  4. RU IPv4    → "DIRECT"
//  5. default    → "PROXY"
//
// Injection points (rendered into the placeholders):
//   - {{LOCAL_PROXY}}        — "host:port" string, the listen address
//   - {{WHITELIST_SUFFIX}}   — shExpMatch OR-expression for suffix patterns
//   - {{WHITELIST_CIDR}}     — isInNet OR-expression for CIDR patterns
//   - {{BLACKLIST_SUFFIX}}   — shExpMatch OR-expression for suffix patterns
//   - {{BLACKLIST_CIDR}}     — isInNet OR-expression for CIDR patterns
//   - {{RU_RANGES}}          — JS array literal of [low, high] pairs
//
// CIDR matching uses isInNet(host, base, mask); the PAC engine resolves
// host → IP internally (same as our local-network checks), so CIDR entries
// work against hostnames and IP literals alike.
const pacTemplate = `var LOCAL_PROXY = "{{LOCAL_PROXY}}";

var RU_RANGES = {{RU_RANGES}};

function isWhitelisted(host) {
  host = host.toLowerCase();
  if ({{WHITELIST_SUFFIX}}) { return true; }
  if ({{WHITELIST_CIDR}}) { return true; }
  return false;
}

function isBlacklisted(host) {
  host = host.toLowerCase();
  if ({{BLACKLIST_SUFFIX}}) { return true; }
  if ({{BLACKLIST_CIDR}}) { return true; }
  return false;
}

function isLocal(host) {
  if (isPlainHostName(host)) {
    return true;
  }
  if (isInNet(host, "10.0.0.0", "255.0.0.0")) { return true; }
  if (isInNet(host, "172.16.0.0", "255.240.0.0")) { return true; }
  if (isInNet(host, "192.168.0.0", "255.255.0.0")) { return true; }
  if (isInNet(host, "127.0.0.0", "255.0.0.0")) { return true; }
  if (isInNet(host, "169.254.0.0", "255.255.0.0")) { return true; }
  return false;
}

// ipToInt converts a dotted-quad IPv4 string to a 32-bit unsigned int.
// Returns -1 on parse failure (non-IPv4 / IPv6), which never matches a range.
function ipToInt(ip) {
  var parts = ip.split(".");
  if (parts.length !== 4) {
    return -1;
  }
  var n = 0;
  var i;
  for (i = 0; i < 4; i++) {
    var octet = parseInt(parts[i], 10);
    if (isNaN(octet) || octet < 0 || octet > 255) {
      return -1;
    }
    n = (n * 256) + octet;
  }
  return n;
}

// inRu performs a binary search over the sorted disjoint RU_RANGES array.
// Returns true iff n falls in any [low, high] pair.
function inRu(n) {
  var lo = 0;
  var hi = RU_RANGES.length - 1;
  while (lo <= hi) {
    var m = (lo + hi) >> 1;
    var r = RU_RANGES[m];
    if (n < r[0]) {
      hi = m - 1;
    } else if (n > r[1]) {
      lo = m + 1;
    } else {
      return true;
    }
  }
  return false;
}

function FindProxyForURL(url, host) {
  // 1. blacklist — absolute veto, overrides everything.
  if (isBlacklisted(host)) {
    return "DIRECT";
  }
  // 2. whitelist — overrides local and geo.
  if (isWhitelisted(host)) {
    return "PROXY " + LOCAL_PROXY;
  }
  // 3. local/private network.
  if (isLocal(host)) {
    return "DIRECT";
  }
  // 4. RU IPv4 — dnsResolve returns a string; IPv6 contains ":" and is
  //    rejected by ipToInt (returns -1), which never matches.
  var ip = dnsResolve(host);
  if (ip && inRu(ipToInt(ip))) {
    return "DIRECT";
  }
  // 5. default.
  return "PROXY " + LOCAL_PROXY;
}
`

// RenderPAC produces the PAC document from the given input. The output is a
// complete, valid PAC file ready to serve at /proxy.pac.
//
// The renderer does NOT validate the input — callers (the app layer) must
// supply a populated PACInput. Empty lists render always-false matchers; an
// empty Ranges list renders an empty RU_RANGES array.
//
// Whitelist/blacklist entries are partitioned by form: CIDR entries (those
// containing a "/" that parses as a valid v4 prefix) become isInNet calls;
// everything else becomes a shExpMatch (suffix) call. Invalid CIDR strings
// are treated as suffix patterns (safer: they'll just never match an IP).
func RenderPAC(in port.PACInput) []byte {
	wlSuffix, wlCIDR := splitMatchers(in.Whitelist)
	blSuffix, blCIDR := splitMatchers(in.Blacklist)

	out := pacTemplate
	out = strings.ReplaceAll(out, "{{LOCAL_PROXY}}", in.Listen)
	out = strings.ReplaceAll(out, "{{WHITELIST_SUFFIX}}", renderShExpMatcher(wlSuffix))
	out = strings.ReplaceAll(out, "{{WHITELIST_CIDR}}", renderIsInNetMatcher(wlCIDR))
	out = strings.ReplaceAll(out, "{{BLACKLIST_SUFFIX}}", renderShExpMatcher(blSuffix))
	out = strings.ReplaceAll(out, "{{BLACKLIST_CIDR}}", renderIsInNetMatcher(blCIDR))
	out = strings.ReplaceAll(out, "{{RU_RANGES}}", renderRangesArray(in.Ranges.Ranges))
	return []byte(out)
}

// splitMatchers partitions a list of patterns into (suffixPatterns, cidrPatterns).
// An entry is a CIDR iff it contains "/" and parses via net.ParseCIDR into a
// valid IPv4 prefix. IPv6 CIDRs are rejected (treated as suffix; v1 is IPv4-only).
func splitMatchers(patterns []string) (suffix, cidr []string) {
	for _, p := range patterns {
		if isIPv4CIDR(p) {
			cidr = append(cidr, p)
		} else {
			suffix = append(suffix, p)
		}
	}
	return suffix, cidr
}

// isIPv4CIDR reports whether s parses as an IPv4 CIDR prefix.
func isIPv4CIDR(s string) bool {
	if !strings.Contains(s, "/") {
		return false
	}
	ip, _, err := net.ParseCIDR(s)
	if err != nil {
		return false
	}
	return ip.To4() != nil
}

// renderShExpMatcher turns a list of suffix patterns into a JavaScript OR
// expression of shExpMatch calls, e.g. ["*.a.com","b.com"] becomes:
//
//	shExpMatch(host, "*.a.com") || shExpMatch(host, "b.com")
//
// An empty list returns "false" so the surrounding `if ({{...}})` is valid.
func renderShExpMatcher(patterns []string) string {
	if len(patterns) == 0 {
		return "false"
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = fmt.Sprintf("shExpMatch(host, %s)", jsString(p))
	}
	return strings.Join(parts, " || ")
}

// renderIsInNetMatcher turns a list of CIDR strings into a JavaScript OR
// expression of isInNet calls. Each CIDR "base/prefix" becomes:
//
//	isInNet(host, "base", "<dotted mask>")
//
// The mask is computed from the prefix length and emitted as a dotted quad
// (PAC engines accept dotted-quad masks, not "/24" form). An empty list
// returns "false".
func renderIsInNetMatcher(cidrs []string) string {
	if len(cidrs) == 0 {
		return "false"
	}
	parts := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			// Defensive: splitMatchers should have filtered these, but
			// never let a bad CIDR break the PAC.
			continue
		}
		mask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])
		base := ipNet.IP.String()
		parts = append(parts, fmt.Sprintf("isInNet(host, %s, %s)", jsString(base), jsString(mask)))
	}
	if len(parts) == 0 {
		return "false"
	}
	return strings.Join(parts, " || ")
}

// renderRangesArray turns a slice of disjoint sorted ranges into a JavaScript
// array literal: [[low, high], [low, high], ...]. Empty input → "[]".
func renderRangesArray(ranges []port.Range) string {
	if len(ranges) == 0 {
		return "[]"
	}
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = "[" + strconv.FormatUint(uint64(r.Low), 10) + ", " + strconv.FormatUint(uint64(r.High), 10) + "]"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// jsString renders a Go string as a double-quoted JavaScript string literal,
// escaping the minimal set: backslash and double-quote. Patterns come from
// config (user-controlled) so we must not allow quote-breakout.
func jsString(s string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
	).Replace(s)
	return `"` + escaped + `"`
}
