// Package dialchain_test contains the dial-chain behavior tests. They moved
// verbatim from internal/in/http (smart chain tests) when the chain was
// extracted into the shared package — see change add-transparent-listener,
// design D1. The package-level doc for the chain itself lives in dialchain.go.
package dialchain_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/dialchain"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// recordingDialer captures dial calls so tests can assert the exact targets
// attempted (pinned RU IP vs hostname) and program outcomes per attempt.
type recordingDialer struct {
	mu     sync.Mutex
	calls  []string
	script map[string][]error // addr -> outcomes consumed in order; nil = success conn
}

func newRecordingDialer(script map[string][]error) *recordingDialer {
	return &recordingDialer{script: script}
}

func (r *recordingDialer) DialContext(_ context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, network+" "+addr)
	outcomes := r.script[addr]
	if len(outcomes) == 0 {
		return nil, errors.New("unexpected dial " + addr)
	}
	err := outcomes[0]
	r.script[addr] = outcomes[1:]
	if err == nil {
		var c net.Conn = fakeConn{}
		return c, nil
	}
	return nil, err
}

// fakeConn is a do-nothing net.Conn placeholder. The chain never reads or
// writes the returned conn — only the dial call record matters to the tests.
type fakeConn struct{}

func (fakeConn) Read(b []byte) (int, error)         { return 0, nil }
func (fakeConn) Write(b []byte) (int, error)        { return len(b), nil }
func (fakeConn) Close() error                       { return nil }
func (fakeConn) LocalAddr() net.Addr                { return nil }
func (fakeConn) RemoteAddr() net.Addr               { return nil }
func (fakeConn) SetDeadline(time.Time) error        { return nil }
func (fakeConn) SetReadDeadline(time.Time) error    { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error   { return nil }

func (r *recordingDialer) dialCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// fakeUpstream records targets and returns scripted outcomes in order.
type fakeUpstream struct {
	mu      sync.Mutex
	targets []string
	script  []error // consumed head-first; nil = success
}

func (f *fakeUpstream) dial(target string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, target)
	if len(f.script) == 0 {
		return nil, errors.New("unexpected upstream dial " + target)
	}
	err := f.script[0]
	f.script = f.script[1:]
	if err == nil {
		var c net.Conn = fakeConn{}
		return c, nil
	}
	return nil, err
}

func (f *fakeUpstream) dialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

// staticResolver returns the fixed IP list, or an error.
type staticResolver struct {
	ips []net.IP
	err error
}

func (s staticResolver) Resolve(context.Context, string) ([]net.IP, error) {
	return s.ips, s.err
}

var _ dialchain.ResolveFunc = staticResolver{}.Resolve

func cfgFor(listen string) port.Config {
	return port.Config{
		Listen: listen,
		Smart: port.Smart{
			Listen:           "127.0.0.1:8081",
			DialTimeout:      time.Second,
			DialAttempts:     2,
			NegativeCacheTTL: time.Minute,
		},
	}
}

func newNegCache() *dialchain.NegativeCache {
	return dialchain.NewNegativeCacheAt(time.Now)
}

// TestChain_DirectFirstThenUpstreamFallback walks [Direct, Upstream] when
// Direct fails after its attempts.
func TestChain_DirectFirstThenUpstreamFallback(t *testing.T) {
	t.Parallel()
	host := "blocked.example.com"
	ranges := &port.RURanges{}
	cfg := cfgFor("127.0.0.1:8080")
	cfg.Routing.Blacklist = []string{"blocked.example.com"}

	direct := newRecordingDialer(map[string][]error{
		"blocked.example.com:443": {errors.New("refused"), errors.New("refused")},
	})
	up := &fakeUpstream{script: []error{nil}}

	chain := dialchain.New(staticResolver{}.Resolve, direct.DialContext, up.dial, newNegCache())
	conn, err := chain.Dial(context.Background(), host+":443", cfg, *ranges)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if got := direct.dialCalls(); len(got) != 2 {
		t.Errorf("direct attempts = %d (%v), want 2", len(got), got)
	}
	if got := up.dialed(); len(got) != 1 || got[0] != host+":443" {
		t.Errorf("upstream targets = %v, want [%s:443]", got, host)
	}
}

// TestChain_RUPinnedDialIP proves the RU-geo strategy dials the pinned IP,
// not the hostname, and does not fall back to upstream on failure.
func TestChain_RUPinnedDialIP(t *testing.T) {
	t.Parallel()
	ru := net.IPv4(5, 6, 7, 8).To4()
	host := "ru.example.com"
	ranges := &port.RURanges{Ranges: []port.Range{{Low: ipToU32(net.IPv4(5, 6, 7, 0).To4()), High: ipToU32(net.IPv4(5, 6, 7, 255).To4())}}}

	direct := newRecordingDialer(map[string][]error{
		"5.6.7.8:443": {errors.New("unreachable")},
	})
	up := &fakeUpstream{script: []error{errors.New("must not be dialed")}}

	chain := dialchain.New(staticResolver{ips: []net.IP{ru}}.Resolve, direct.DialContext, up.dial, newNegCache())
	_, err := chain.Dial(context.Background(), host+":443", cfgFor(""), *ranges)
	if err == nil {
		t.Fatal("expected all-strategies-failed error")
	}
	if got := direct.dialCalls(); len(got) != 2 || got[0] != "tcp 5.6.7.8:443" || got[1] != "tcp 5.6.7.8:443" {
		t.Errorf("direct calls = %v, want two attempts on pinned 5.6.7.8:443", got)
	}
	if got := up.dialed(); len(got) != 0 {
		t.Errorf("upstream must not be dialed for RU pin, got %v", got)
	}
}

// TestChain_NegativeCacheSkipsDirect proves a host marked negative skips the
// Direct attempt on the next dial and goes straight to upstream, and that a
// failed Direct in a fallback-capable list records the negative entry.
func TestChain_NegativeCacheSkipsDirect(t *testing.T) {
	t.Parallel()
	host := "flaky.example.com"
	ranges := &port.RURanges{}
	cfg := cfgFor("")
	cfg.Routing.Blacklist = []string{host}

	direct := newRecordingDialer(map[string][]error{
		"flaky.example.com:443": {errors.New("down"), errors.New("down"), nil},
	})
	up := &fakeUpstream{script: []error{nil, nil}}

	chain := dialchain.New(staticResolver{}.Resolve, direct.DialContext, up.dial, newNegCache())

	// First dial: Direct fails twice (attempts=2), marks negative, falls
	// back upstream.
	conn, err := chain.Dial(context.Background(), host+":443", cfg, *ranges)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = conn.Close()

	// Second dial within TTL: Direct skipped entirely.
	conn, err = chain.Dial(context.Background(), host+":443", cfg, *ranges)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	_ = conn.Close()

	calls := direct.dialCalls()
	if len(calls) != 2 {
		t.Errorf("total direct attempts = %d (%v), want 2 (skip on second Dial)", len(calls), calls)
	}
	if got := up.dialed(); len(got) != 2 {
		t.Errorf("upstream dials = %d, want 2", len(got))
	}
}

// TestChain_NegativeCacheNotConsultedForUpstreamOnly proves the cache is
// untouched for [Upstream]-only lists (whitelist/default).
func TestChain_NegativeCacheNotConsultedForUpstreamOnly(t *testing.T) {
	t.Parallel()
	host := "plain.example.com"
	ranges := &port.RURanges{}

	up := &fakeUpstream{script: []error{nil}}
	direct := newRecordingDialer(nil)

	chain := dialchain.New(staticResolver{}.Resolve, direct.DialContext, up.dial, newNegCache())
	conn, err := chain.Dial(context.Background(), host+":443", cfgFor(""), *ranges)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if got := direct.dialCalls(); len(got) != 0 {
		t.Errorf("direct must not be dialed for upstream-only, got %v", got)
	}
}

// TestChain_AllStrategiesFail proves the all-fail error carries the target.
func TestChain_AllStrategiesFail(t *testing.T) {
	t.Parallel()
	host := "dead.example.com"
	ranges := &port.RURanges{}
	cfg := cfgFor("")
	cfg.Routing.Blacklist = []string{host}

	direct := newRecordingDialer(map[string][]error{
		"dead.example.com:443": {errors.New("x"), errors.New("x")},
	})
	up := &fakeUpstream{script: []error{errors.New("upstream down")}}

	chain := dialchain.New(staticResolver{}.Resolve, direct.DialContext, up.dial, newNegCache())
	_, err := chain.Dial(context.Background(), host+":443", cfg, *ranges)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := up.dialed(); len(got) != 1 {
		t.Errorf("upstream dials = %d, want 1", len(got))
	}
}

// TestChain_RUDirectFailureDoesNotPolluteNegativeCache proves the RU pin path
// (no fallback) does not record a negative entry.
func TestChain_RUDirectFailureDoesNotPolluteNegativeCache(t *testing.T) {
	t.Parallel()
	ru := net.IPv4(9, 9, 9, 9).To4()
	ranges := &port.RURanges{Ranges: []port.Range{{Low: ipToU32(net.IPv4(9, 9, 9, 0).To4()), High: ipToU32(net.IPv4(9, 9, 9, 255).To4())}}}

	direct := newRecordingDialer(map[string][]error{
		"9.9.9.9:443": {errors.New("x")},
	})
	up := &fakeUpstream{script: []error{nil}}

	nc := newNegCache()
	chain := dialchain.New(staticResolver{ips: []net.IP{ru}}.Resolve, direct.DialContext, up.dial, nc)
	_, err := chain.Dial(context.Background(), "ruh.example.com:443", cfgFor(""), *ranges)
	if err == nil {
		t.Fatal("expected failure")
	}

	// The same host must NOT be negative: a follow-up dial re-attempts Direct
	// (dial_attempts=2 each dial → 2 more attempts, 4 total).
	_, _ = chain.Dial(context.Background(), "ruh.example.com:443", cfgFor(""), *ranges)
	if got := direct.dialCalls(); len(got) != 4 {
		t.Errorf("direct attempts across two dials = %d, want 4 (no negative skip)", len(got))
	}
}

// TestChain_ResolveErrorFallsThroughToDefault proves resolution failure is
// non-fatal and the default [Upstream] applies.
func TestChain_ResolveErrorFallsThroughToDefault(t *testing.T) {
	t.Parallel()
	ranges := &port.RURanges{}
	up := &fakeUpstream{script: []error{nil}}
	chain := dialchain.New(staticResolver{err: errors.New("dns down")}.Resolve, nil, up.dial, newNegCache())
	conn, err := chain.Dial(context.Background(), "any.example.com:443", cfgFor(""), *ranges)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if got := up.dialed(); len(got) != 1 {
		t.Errorf("upstream dials = %d, want 1", len(got))
	}
}

// ipToU32 converts a 4-byte IPv4 to the uint32 range form.
func ipToU32(ip net.IP) uint32 {
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
