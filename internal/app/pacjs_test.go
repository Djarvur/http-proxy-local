package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// pacCase is a single precedence assertion: for a given (url, host, dns)
// input, the PAC's FindProxyForURL MUST return want.
type pacCase struct {
	url  string
	host string
	dns  string // what dnsResolve returns; "" means null (resolution failure)
	want string
}

// runPACHarness writes the rendered PAC to a temp file, spawns node with the
// harness, feeds all cases via stdin, and returns the results in order.
//
// Skips the test if node is unavailable (CI installs it via mise; locally it's
// expected per task 4.6).
func runPACHarness(t *testing.T, pac []byte, cases []pacCase) []string {
	t.Helper()

	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; skipping PAC execution harness (install node or use the goja fallback)")
	}

	dir := t.TempDir()
	pacPath := filepath.Join(dir, "proxy.pac")
	if err := os.WriteFile(pacPath, pac, 0o600); err != nil {
		t.Fatalf("write pac: %v", err)
	}

	harnessPath := filepath.Join("testdata", "pac_harness.js")

	var stdin bytes.Buffer
	enc := json.NewEncoder(&stdin)
	for _, c := range cases {
		tc := map[string]any{"url": c.url, "host": c.host}
		if c.dns != "" {
			tc["dns"] = c.dns
		} else {
			tc["dns"] = nil
		}
		if err := enc.Encode(tc); err != nil {
			t.Fatalf("encode case: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// gosec G204 is a false positive here: "node" is a hardcoded literal, and
	// the variable args (harnessPath is a fixed testdata path, pacPath is the
	// test's own temp file) are not attacker-controlled. Suppress with
	// justification rather than weakening the test.
	cmd := exec.CommandContext(ctx, "node", harnessPath, pacPath) //nolint:gosec // G204: hardcoded binary, test-owned args
	cmd.Stdin = &stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node harness failed: %v\nstderr:\n%s", err, stderr.String())
	}

	out := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(out) != len(cases) {
		t.Fatalf("expected %d results, got %d:\n%s", len(cases), len(out), stdout.String())
	}
	return out
}

// TestPACPrecedence_AllScenarios runs every precedence case from the
// pac-generation spec through the node harness. This is the keystone test:
// if any of these fail, the PAC is wrong.
func TestPACPrecedence_AllScenarios(t *testing.T) {
	t.Parallel()

	// A small but representative RU range: 1.2.3.0/24 = [16909056, 16909311].
	const ruIP = "1.2.3.50"
	const nonRuIP = "8.8.8.8"

	pac := RenderPAC(port.PACInput{
		Listen:    "127.0.0.1:8080",
		Whitelist: []string{"*.whitelisted.com", "10.0.0.0/8"},
		Blacklist: []string{"*.blacklisted.com"},
		Ranges:    port.RURanges{Ranges: []port.Range{{Low: 16909056, High: 16909311}}},
	})

	cases := []pacCase{
		// 1. blacklist overrides everything (whitelist, local, RU, default).
		{url: "http://x", host: "x.blacklisted.com", dns: nonRuIP, want: "DIRECT"},
		{url: "http://x", host: "y.blacklisted.com", dns: ruIP, want: "DIRECT"},

		// 2. whitelist overrides local-network and RU.
		{url: "http://x", host: "api.whitelisted.com", dns: nonRuIP, want: "PROXY 127.0.0.1:8080"},
		// Whitelist CIDR 10.0.0.0/8 forces a local address to PROXY.
		{url: "http://x", host: "10.0.0.5", dns: "", want: "PROXY 127.0.0.1:8080"},
		// Whitelist overrides RU geo.
		{url: "http://x", host: "api.whitelisted.com", dns: ruIP, want: "PROXY 127.0.0.1:8080"},

		// 3. local/private network (not in any list) → DIRECT.
		// NOTE: 10.x matches whitelist CIDR 10.0.0.0/8 above, so it routes PROXY
		// (covered by the whitelist cases). Use other RFC1918 ranges here.
		{url: "http://x", host: "192.168.1.1", dns: "", want: "DIRECT"},
		{url: "http://x", host: "172.16.0.1", dns: "", want: "DIRECT"},
		{url: "http://x", host: "127.0.0.1", dns: "", want: "DIRECT"},
		{url: "http://x", host: "intranet", dns: "", want: "DIRECT"}, // plain hostname (no dots)

		// 4. RU IPv4 (not in any list) → DIRECT.
		{url: "http://x", host: "example.ru", dns: ruIP, want: "DIRECT"},

		// 5. non-RU IPv4 (not in any list) → PROXY.
		{url: "http://x", host: "example.com", dns: nonRuIP, want: "PROXY 127.0.0.1:8080"},

		// Suffix match: blacklist *.blacklisted.com catches subdomains.
		{url: "http://x", host: "deep.sub.blacklisted.com", dns: nonRuIP, want: "DIRECT"},

		// IPv6 resolution: contains ":", ipToInt returns -1, no RU match → PROXY (if not listed).
		{url: "http://x", host: "ipv6.example.com", dns: "::1", want: "PROXY 127.0.0.1:8080"},

		// DNS resolution failure (null): host not in any list, not local literal → PROXY.
		{url: "http://x", host: "unresolved.example.com", dns: "", want: "PROXY 127.0.0.1:8080"},

		// Empty-RU degradation is covered separately (TestPACPrecedence_EmptyRanges).
	}

	results := runPACHarness(t, pac, cases)
	for i, c := range cases {
		got := struct {
			Result string `json:"result"`
			Error  string `json:"error"`
		}{}
		if err := json.Unmarshal([]byte(results[i]), &got); err != nil {
			t.Errorf("case %d (%s): bad harness output %q: %v", i, c.host, results[i], err)
			continue
		}
		if got.Error != "" {
			t.Errorf("case %d host=%q dns=%q: harness error: %s", i, c.host, c.dns, got.Error)
			continue
		}
		if got.Result != c.want {
			t.Errorf("case %d host=%q dns=%q: got %q, want %q", i, c.host, c.dns, got.Result, c.want)
		}
	}
}

// TestPACPrecedence_EmptyRanges verifies graceful degradation: with no RU
// data, even a RU host falls through to PROXY.
func TestPACPrecedence_EmptyRanges(t *testing.T) {
	t.Parallel()
	pac := RenderPAC(port.PACInput{
		Listen: "127.0.0.1:8080",
		// No ranges.
	})
	cases := []pacCase{
		{url: "http://x", host: "would-be-ru.ru", dns: "1.2.3.50", want: "PROXY 127.0.0.1:8080"},
	}
	results := runPACHarness(t, pac, cases)
	var got struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal([]byte(results[0]), &got)
	if got.Result != cases[0].want {
		t.Errorf("empty-ranges degradation: got %q, want %q", got.Result, cases[0].want)
	}
}
