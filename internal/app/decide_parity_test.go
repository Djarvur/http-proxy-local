package app

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// TestDecide_ParityWithPAC is the parity guard (design D9). For a fixed corpus
// of hosts and a fixed config+ranges, the top-priority strategy Decide returns
// MUST match the decision the rendered PAC would make:
//
//   - PAC "DIRECT"          ↔ Decide's first strategy is StrategyDirect
//   - PAC "PROXY <listen>"  ↔ Decide's first strategy is StrategyUpstream
//
// This catches drift between the PAC renderer (pac.go) and Decide (decide.go).
// The PAC is evaluated by the same node harness used by pacjs_test.go, so the
// comparison is against the real PAC bytes, not a re-implementation.
//
// Skipped when node is unavailable (matches runPACHarness's skip policy).
func TestDecide_ParityWithPAC(t *testing.T) {
	t.Parallel()

	const ruIP = "1.2.3.50" // in 1.2.3.0/24 = [16909056, 16909311]
	const nonRuIP = "8.8.8.8"
	const listen = "127.0.0.1:8080"

	cfg := port.Config{
		Listen: listen,
		Routing: port.Routing{
			Whitelist: []string{"*.whitelisted.com", "10.0.0.0/8"},
			Blacklist: []string{"*.blacklisted.com", "192.168.0.0/16"},
		},
	}
	ranges := port.RURanges{Ranges: []port.Range{{Low: 16909056, High: 16909311}}}

	pac := RenderPAC(port.PACInput{
		Listen:    listen,
		Whitelist: cfg.Routing.Whitelist,
		Blacklist: cfg.Routing.Blacklist,
		Ranges:    ranges,
	})

	// Each case: host + dns (the IP the PAC's dnsResolve returns, AND the IP
	// Decide sees in ips). "wantFirstStrategy" is derived from PAC's answer,
	// so we leave it empty here and assert agreement instead.
	cases := []struct {
		host string
		dns  string // "" = resolution failure
	}{
		// blacklist overrides everything
		{host: "x.blacklisted.com", dns: nonRuIP},
		{host: "deep.sub.blacklisted.com", dns: ruIP},
		// blacklist CIDR: 192.168.0.0/16 forces a private addr to DIRECT
		{host: "192.168.1.1", dns: ""},
		// whitelist overrides local+geo
		{host: "api.whitelisted.com", dns: nonRuIP},
		{host: "api.whitelisted.com", dns: ruIP},
		// whitelist CIDR 10.0.0.0/8 forces 10.x to PROXY
		{host: "10.0.0.5", dns: ""},
		// local/private (not in any list)
		{host: "172.16.0.1", dns: ""},
		{host: "127.0.0.1", dns: ""},
		{host: "intranet", dns: ""}, // plain hostname
		// RU geo
		{host: "example.ru", dns: ruIP},
		// non-RU default
		{host: "example.com", dns: nonRuIP},
		// IPv6 resolution → not matched by ipToInt → falls through
		{host: "ipv6.example.com", dns: "::1"},
		// DNS failure on a non-listed host → default
		{host: "unresolved.example.com", dns: ""},
	}

	// Build the node-harness inputs and run the real PAC.
	pacCases := make([]pacCase, len(cases))
	for i, c := range cases {
		pacCases[i] = pacCase{url: "http://x", host: c.host, dns: c.dns}
	}
	results := runPACHarness(t, pac, pacCases)

	for i, c := range cases {
		// Reconstruct the IPs Decide sees: a non-empty dns becomes a single
		// resolved IP; empty dns means no resolution (nil slice). For literal
		// IP hostnames the PAC's resolveToIp uses the literal itself, but
		// Decide is driven by the ips argument, so feed the literal when dns
		// is empty AND the host parses as an IP.
		var decideIPs []net.IP
		if c.dns != "" {
			decideIPs = []net.IP{net.ParseIP(c.dns)}
		} else if ip := net.ParseIP(c.host); ip != nil {
			decideIPs = []net.IP{ip}
		}

		gotStrategies := Decide(c.host, decideIPs, cfg, ranges)
		if len(gotStrategies) == 0 {
			t.Fatalf("case %d (%s): Decide returned no strategies", i, c.host)
		}
		gotFirst := gotStrategies[0].Path

		// Parse the PAC's result for this case.
		var pacResult struct {
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal([]byte(results[i]), &pacResult); err != nil {
			t.Fatalf("case %d (%s): bad harness output %q: %v", i, c.host, results[i], err)
		}
		if pacResult.Error != "" {
			t.Fatalf("case %d (%s): PAC harness error: %s", i, c.host, pacResult.Error)
		}

		wantFirst := strategyFromPAC(t, pacResult.Result)
		if gotFirst != wantFirst {
			t.Errorf("case %d host=%q dns=%q: Decide first strategy = %s, PAC says %q (→ %s)",
				i, c.host, c.dns, strategyName(gotFirst), pacResult.Result, strategyName(wantFirst))
		}
	}
}

// strategyFromPAC maps a PAC result string to the StrategyPath Decide's first
// strategy should have, for parity.
func strategyFromPAC(t *testing.T, pacResult string) StrategyPath {
	t.Helper()
	switch {
	case pacResult == "DIRECT":
		return StrategyDirect
	case len(pacResult) >= 5 && pacResult[:5] == "PROXY":
		return StrategyUpstream
	default:
		t.Fatalf("unexpected PAC result %q", pacResult)
		return 0
	}
}

// strategyName renders a StrategyPath for readable failure messages.
func strategyName(p StrategyPath) string {
	switch p {
	case StrategyDirect:
		return "Direct"
	case StrategyUpstream:
		return "Upstream"
	default:
		return "?"
	}
}
