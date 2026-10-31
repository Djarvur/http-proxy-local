package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	httpserver "github.com/Djarvur/http-proxy-local/internal/in/http"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// These tests exercise serveBoth — the listener-coordination helper that
// runs the HTTP server and (optionally) the SOCKS5 listener, tearing down
// both on context cancel or either-listener-failure. They live in package
// main (not main_test) because serveBoth is unexported; the coordination
// logic is an internal concern of the composition root.

// TestServeBoth_HTTPOnly proves the helper runs the HTTP server alone when
// the SOCKS5 listener is nil (the socks5 block is absent or empty — full
// backward compatibility). Cancelling the context shuts the HTTP server down.
func TestServeBoth_HTTPOnly(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	body := []byte("pac")
	pacPtr.Store(&body)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen: %v", err)
	}
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, srv, httpLn, nil, nil, nil, cfgPtr) }()

	if !probeURL(t, "http://"+httpLn.Addr().String()+httpserver.PACPath) {
		t.Fatal("HTTP listener never became ready")
	}

	cancel()
	select {
	case err := <-done:
		// http.ErrServerClosed is the expected normal-shutdown return.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serveBoth returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoth did not return within 5s of cancel (HTTP-only mode)")
	}
}

// TestServeBoth_HTTPAndSOCKS5 proves both listeners run concurrently and
// both are closed on context cancel.
func TestServeBoth_HTTPAndSOCKS5(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	body := []byte("pac")
	pacPtr.Store(&body)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen: %v", err)
	}
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}

	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, srv, httpLn, socksLn, nil, nil, cfgPtr) }()

	if !probeDial(t, httpLn.Addr().String(), 2*time.Second) {
		t.Fatal("HTTP listener never accepted")
	}
	if !probeDial(t, socksLn.Addr().String(), 2*time.Second) {
		t.Fatal("SOCKS5 listener never accepted")
	}

	cancel()
	select {
	case e := <-done:
		_ = e // serveAll returns nil on clean ctx cancel; only wakeup matters
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoth did not return within 5s of cancel (HTTP+SOCKS5 mode)")
	}

	if !isClosed(httpLn) {
		t.Error("HTTP listener not closed after shutdown")
	}
	if !isClosed(socksLn) {
		t.Error("SOCKS5 listener not closed after shutdown")
	}
}

// TestServeBoth_EitherListenerFailsTearsDownOther proves the design D2
// contract: if the SOCKS5 listener fails (its goroutine returns), the HTTP
// server is also shut down and serveBoth returns. A half-broken process is
// worse than a clean exit.
func TestServeBoth_EitherListenerFailsTearsDownOther(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	body := []byte("pac")
	pacPtr.Store(&body)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen: %v", err)
	}
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}

	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, srv, httpLn, socksLn, nil, nil, cfgPtr) }()

	// Wait for serveBoth to start serving so the close actually interrupts
	// an Accept rather than racing startup.
	if !probeDial(t, socksLn.Addr().String(), 2*time.Second) {
		t.Fatal("SOCKS5 listener never accepted before forced close")
	}

	// Force the SOCKS5 listener to fail: close it out from under Serve.
	// The next Accept errors; socks5.Serve returns; serveBoth must tear
	// down the HTTP server in response.
	_ = socksLn.Close()

	select {
	case e := <-done:
		_ = e // serveBoth returned because SOCKS5 failed; error content unused here
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoth did not return after SOCKS5 listener failed")
	}
}

// probeURL polls a URL until it returns any response or the deadline passes.
func probeURL(t *testing.T, target string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(target) //nolint:gosec // test probe to a localhost test server
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// probeDial polls an address until a TCP connection succeeds or the deadline
// passes.
func probeDial(t *testing.T, addr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// isClosed reports whether a listener has been closed (Accept returns an
// error). Used to assert shutdown.
func isClosed(ln net.Listener) bool {
	_, err := ln.Accept()
	return err != nil
}

// TestServeAll_HTTPAndSmart proves the HTTP and smart HTTP listeners run
// concurrently, both accept requests, and both shut down on context cancel.
// SOCKS5 is nil here (the smart listener is independent of SOCKS5).
func TestServeAll_HTTPAndSmart(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	body := []byte("pac")
	pacPtr.Store(&body)
	rangesPtr := &atomic.Pointer[port.RURanges]{}
	emptyRanges := port.RURanges{}
	rangesPtr.Store(&emptyRanges)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen: %v", err)
	}
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	smartLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smart listen: %v", err)
	}
	smartSrv := &http.Server{
		Handler:           httpserver.NewSmart(cfgPtr, rangesPtr, net.DefaultResolver, httpserver.NewNegativeCache()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, srv, httpLn, nil, smartSrv, smartLn, cfgPtr) }()

	// Both listeners must accept.
	if !probeURL(t, "http://"+httpLn.Addr().String()+httpserver.PACPath) {
		t.Fatal("HTTP listener never became ready")
	}
	// Smart listener rejects /proxy.pac with 404 — that still proves it's up.
	if !probeURL(t, "http://"+smartLn.Addr().String()+httpserver.PACPath) {
		t.Fatal("smart listener never became ready")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serveAll returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveAll did not return within 5s of cancel (HTTP+smart mode)")
	}

	if !isClosed(smartLn) {
		t.Error("smart listener not closed after shutdown")
	}
}

// TestServeAll_SmartAbsentNoBind proves that when smartSrv is nil, no smart
// listener is bound and behavior is identical to the pre-smart-listener build.
func TestServeAll_SmartAbsentNoBind(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[port.Config]{}
	pacPtr := &atomic.Pointer[[]byte]{}
	body := []byte("pac")
	pacPtr.Store(&body)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen: %v", err)
	}
	srv := &http.Server{
		Handler:           httpserver.New(cfgPtr, pacPtr),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// smartSrv and smartLn both nil → smart disabled.
	go func() { done <- serveAll(ctx, srv, httpLn, nil, nil, nil, cfgPtr) }()

	if !probeURL(t, "http://"+httpLn.Addr().String()+httpserver.PACPath) {
		t.Fatal("HTTP listener never became ready")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serveAll returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveAll did not return within 5s of cancel (HTTP-only mode)")
	}
}
