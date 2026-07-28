package app

import (
	"net"
	"reflect"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// TestDecide exercises the pure routing-decision function over the full
// precedence table. Decide mirrors the PAC's precedence but emits an ordered
// strategy list (with fallback for the blacklist rule) instead of a single
// DIRECT/PROXY answer.
func TestDecide(t *testing.T) {
	t.Parallel()

	// A small but representative RU range: 1.2.3.0/24 = [16909056, 16909311].
	ruRanges := port.RURanges{Ranges: []port.Range{{Low: 16909056, High: 16909311}}}
	const ruIP = "1.2.3.50"
	const nonRuIP = "8.8.8.8"

	cfg := port.Config{
		Routing: port.Routing{
			Whitelist: []string{"*.whitelisted.com", "10.0.0.0/8"},
			Blacklist: []string{"*.blacklisted.com", "10.0.0.0/8"},
		},
	}

	// Helper to build []net.IP from dotted-quad strings.
	ips := func(addrs ...string) []net.IP {
		out := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.ParseIP(a))
		}
		return out
	}

	tests := []struct {
		name string
		host string
		ips  []net.IP
		want []Strategy
	}{
		// 1. blacklist (suffix) → [Direct, Upstream].
		{name: "blacklist suffix", host: "x.blacklisted.com", ips: ips(nonRuIP), want: []Strategy{{Path: StrategyDirect}, {Path: StrategyUpstream}}},
		{name: "blacklist suffix deep subdomain", host: "deep.sub.blacklisted.com", ips: ips(nonRuIP), want: []Strategy{{Path: StrategyDirect}, {Path: StrategyUpstream}}},

		// 1b. blacklist (CIDR) → [Direct, Upstream]. 172.16.0.5 is in the
		// 10.0.0.0/8 entry? No — 172.16 is not in 10/8. Use 10.0.0.5.
		{name: "blacklist CIDR", host: "host.example.com", ips: ips("10.0.0.5"), want: []Strategy{{Path: StrategyDirect}, {Path: StrategyUpstream}}},

		// 2. whitelist (suffix) → [Upstream]. (Host NOT in blacklist.)
		{name: "whitelist suffix", host: "api.whitelisted.com", ips: ips(nonRuIP), want: []Strategy{{Path: StrategyUpstream}}},
		// 2b. whitelist (CIDR) → [Upstream]. Use a CIDR that's in whitelist but
		// not blacklist to isolate the rule. Rebuild cfg below for this case.

		// 3. plain hostname → [Direct] (no DNS needed).
		{name: "plain hostname", host: "intranet", ips: nil, want: []Strategy{{Path: StrategyDirect}}},

		// 3b. private IP (not in any list) → [Direct].
		{name: "private 192.168", host: "192.168.1.1", ips: ips("192.168.1.1"), want: []Strategy{{Path: StrategyDirect}}},
		{name: "private 172.16", host: "172.16.0.1", ips: ips("172.16.0.1"), want: []Strategy{{Path: StrategyDirect}}},
		{name: "private 127", host: "127.0.0.1", ips: ips("127.0.0.1"), want: []Strategy{{Path: StrategyDirect}}},
		{name: "private 169.254", host: "169.254.1.1", ips: ips("169.254.1.1"), want: []Strategy{{Path: StrategyDirect}}},

		// 4. RU IPv4 → [Direct] pinned to the matched RU IP.
		{name: "RU IPv4 in range", host: "example.ru", ips: ips(ruIP), want: []Strategy{{Path: StrategyDirect, DialIP: net.ParseIP(ruIP)}}},

		// 4b. RU IPv4 not in range → default.
		{name: "non-RU IPv4 falls through", host: "example.com", ips: ips(nonRuIP), want: []Strategy{{Path: StrategyUpstream}}},

		// 5. default catch-all → [Upstream].
		{name: "default upstream", host: "example.com", ips: ips(nonRuIP), want: []Strategy{{Path: StrategyUpstream}}},

		// 6. blacklist+whitelist both match → blacklist wins.
		// *.blacklisted.com is only in blacklist; need a host in both. Use the
		// CIDR 10.0.0.0/8 which is in BOTH lists, with a 10.x IP.
		{name: "blacklist overrides whitelist via shared CIDR", host: "host.example.com", ips: ips("10.0.0.5"), want: []Strategy{{Path: StrategyDirect}, {Path: StrategyUpstream}}},

		// 7. multiple resolved IPs with one RU → first RU IP wins (DialIP set).
		{name: "multiple IPs first RU wins", host: "cdn.example", ips: ips(nonRuIP, ruIP, "9.9.9.9"), want: []Strategy{{Path: StrategyDirect, DialIP: net.ParseIP(ruIP)}}},

		// 8. IPv6-only resolved IPs → geo does not match, falls through to default.
		{name: "IPv6-only falls through", host: "ipv6.example.com", ips: ips("::1"), want: []Strategy{{Path: StrategyUpstream}}},

		// 9. no resolved IPs, host not in any list, not plain → default.
		{name: "no IPs default", host: "unresolved.example.com", ips: nil, want: []Strategy{{Path: StrategyUpstream}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tt.host, tt.ips, cfg, ruRanges)
			if !strategiesEqual(got, tt.want) {
				t.Errorf("Decide(%q, %v) = %s, want %s", tt.host, tt.ips, strategiesStr(got), strategiesStr(tt.want))
			}
		})
	}
}

// TestDecide_WhitelistCIDROnly isolates the whitelist-CIDR rule by using a cfg
// whose blacklist does NOT contain the same CIDR (otherwise blacklist would
// win). Confirms whitelist CIDR yields [Upstream].
func TestDecide_WhitelistCIDROnly(t *testing.T) {
	t.Parallel()
	ruRanges := port.RURanges{Ranges: []port.Range{{Low: 16909056, High: 16909311}}}
	cfg := port.Config{
		Routing: port.Routing{
			Whitelist: []string{"172.16.0.0/12"},
			Blacklist: []string{"*.blacklisted.com"},
		},
	}
	got := Decide("host.example.com", []net.IP{net.ParseIP("172.16.0.5")}, cfg, ruRanges)
	want := []Strategy{{Path: StrategyUpstream}}
	if !strategiesEqual(got, want) {
		t.Errorf("Decide whitelist-CIDR-only = %s, want %s", strategiesStr(got), strategiesStr(want))
	}
}

// TestDecide_RUPinSelectsFirstRU confirms the pinned DialIP is the FIRST RU IP
// encountered in the resolved slice, not just any RU IP.
func TestDecide_RUPinSelectsFirstRU(t *testing.T) {
	t.Parallel()
	ruRanges := port.RURanges{Ranges: []port.Range{{Low: 16909056, High: 16909311}}}
	cfg := port.Config{}
	// Two RU IPs in range; the first (1.2.3.10) must be selected.
	got := Decide("host.example", []net.IP{net.ParseIP("1.2.3.10"), net.ParseIP("1.2.3.20")}, cfg, ruRanges)
	want := []Strategy{{Path: StrategyDirect, DialIP: net.ParseIP("1.2.3.10")}}
	if !strategiesEqual(got, want) {
		t.Errorf("Decide RU pin = %s, want %s", strategiesStr(got), strategiesStr(want))
	}
}

// strategiesEqual compares two strategy slices by Path and DialIP bytes.
func strategiesEqual(a, b []Strategy) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].Path != b[i].Path {
			return false
		}
		if !reflect.DeepEqual(a[i].DialIP, b[i].DialIP) {
			return false
		}
	}
	return true
}

// strategiesStr renders a strategy slice for readable failure messages.
func strategiesStr(s []Strategy) string {
	out := "{"
	for i, st := range s {
		if i > 0 {
			out += ", "
		}
		name := "?"
		switch st.Path {
		case StrategyDirect:
			name = "Direct"
		case StrategyUpstream:
			name = "Upstream"
		}
		if st.DialIP != nil {
			name += "(" + st.DialIP.String() + ")"
		}
		out += name
	}
	return out + "}"
}
