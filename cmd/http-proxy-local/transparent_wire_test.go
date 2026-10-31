package main

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// mustDummyLn returns a bound throwaway listener standing in for the
// always-present dumb HTTP listener.
func mustDummyLn(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// TestBindOptInListeners_TransparentDisabledByDefault proves the transparent
// listener binds nothing when the block is absent or empty (backward compat),
// and transparentStop is a safe no-op.
func TestBindOptInListeners_TransparentDisabledByDefault(t *testing.T) {
	t.Parallel()

	ln := mustDummyLn(t)
	t.Cleanup(func() { _ = ln.Close() })

	for _, tc := range []struct {
		name string
		cfg  port.Config
	}{
		{"block absent", port.Config{Listen: "127.0.0.1:18080"}},
		{"listen empty", port.Config{Listen: "127.0.0.1:18080", Transparent: port.Transparent{Listen: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := bindOptInListeners(tc.cfg, &atomic.Pointer[port.Config]{}, &atomic.Pointer[port.RURanges]{}, ln)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if res.transparentLn != nil {
				t.Errorf("transparent listener bound for %s", tc.name)
			}
			res.transparentStop() // must be a safe no-op when disabled
		})
	}
}

// TestBindOptInListeners_TransparentBindsWhenSet proves the listener binds
// the configured address when enabled on a Linux platform.
func TestBindOptInListeners_TransparentBindsWhenSet(t *testing.T) {
	if !transparentSupported() {
		t.Skip("transparent interception requires Linux")
	}
	t.Parallel()

	ln := mustDummyLn(t)
	t.Cleanup(func() { _ = ln.Close() })

	cfg := port.Config{Listen: "127.0.0.1:18080", Transparent: port.Transparent{Listen: "127.0.0.1:0"}}
	res, err := bindOptInListeners(cfg, &atomic.Pointer[port.Config]{}, &atomic.Pointer[port.RURanges]{}, ln)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(res.transparentStop)

	if res.transparentLn == nil {
		t.Fatal("transparent listener not bound despite non-empty transparent.listen")
	}
}

// TestBindOptInListeners_TransparentRefusedOnNonLinux proves the platform
// guard: a non-empty transparent.listen on an unsupported platform is a
// startup FAILURE naming the limitation (design D6), and the dumb listener
// is closed so nothing leaks.
func TestBindOptInListeners_TransparentRefusedOnNonLinux(t *testing.T) {
	if transparentSupported() {
		t.Skip("refusal only observable on non-Linux platforms")
	}
	t.Parallel()

	ln := mustDummyLn(t)

	cfg := port.Config{Listen: "127.0.0.1:18080", Transparent: port.Transparent{Listen: "127.0.0.1:12345"}}
	_, err := bindOptInListeners(cfg, &atomic.Pointer[port.Config]{}, &atomic.Pointer[port.RURanges]{}, ln)
	if err == nil {
		t.Fatal("expected a startup error on a non-Linux platform")
	}
	if !isClosed(ln) {
		t.Error("dumb HTTP listener must be closed on bind failure")
	}
}
