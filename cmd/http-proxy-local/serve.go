package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/socks5"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// shutdownTimeout is the budget for gracefully shutting down the HTTP server
// on context cancellation. Matches the design D2 coordination budget.
const shutdownTimeout = 10 * time.Second

// serveBoth runs the HTTP server on httpLn and (when socksLn != nil) the
// SOCKS5 listener concurrently, blocking until ctx is cancelled OR either
// listener exits. On exit it ensures both listeners are closed and the HTTP
// server is shut down.
//
// Coordination contract (design D2): either listener failing unexpectedly
// tears down the other. A half-broken process is worse than a clean exit —
// if SOCKS5 dies but HTTP keeps serving, the operator won't notice the
// silent degradation; surfacing the failure forces a restart and a log line.
//
// Returns the error from whichever listener failed first (nil on clean
// shutdown via ctx cancellation). http.ErrServerClosed is the expected
// return for the normal-shutdown path and is NOT treated as an error by
// callers.
func serveBoth(
	ctx context.Context,
	srv *http.Server,
	httpLn net.Listener,
	socksLn net.Listener,
	cfgPtr *atomic.Pointer[port.Config],
) error {
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Serve(httpLn) }()

	var socksDone chan error
	if socksLn != nil {
		socksDone = make(chan error, 1)
		go func() {
			socks5.Serve(ctx, socksLn, cfgPtr)
			// socks5.Serve returns when its context is cancelled OR the
			// listener is closed. It does not return a listener error —
			// Accept errors are logged and treated as shutdown inside Serve.
			// We signal completion with nil so the select below can branch on
			// channel-closed semantics.
			socksDone <- nil
		}()
	}

	// Wait for shutdown trigger: ctx cancellation, or either listener exiting.
	select {
	case <-ctx.Done():
		return shutdownAll(srv, socksLn, httpDone, socksDone)

	case err := <-httpDone:
		// HTTP listener exited unexpectedly. Tear down SOCKS5 (if running)
		// and close everything. The HTTP error is the one we surface.
		log.Printf("http listener exited: %v", err)
		_ = socksLnClose(socksLn)
		_ = socksDoneWait(socksDone)
		return err

	case <-socksDoneChan(socksDone):
		// SOCKS5 listener exited unexpectedly. Tear down HTTP.
		log.Printf("socks5 listener exited")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-httpDone
		return errors.New("socks5 listener exited unexpectedly")
	}
}

// shutdownAll is the clean-shutdown path: srv.Shutdown + close both listeners.
// The HTTP listener is closed by srv.Shutdown; socksLn (if non-nil) is closed
// here to unblock socks5.Serve's Accept loop.
func shutdownAll(
	srv *http.Server,
	socksLn net.Listener,
	httpDone <-chan error,
	socksDone <-chan error,
) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-httpDone
	// Closing socksLn is what unblocks socks5.Serve's Accept loop (its own
	// ctx-cancel goroutine also closes it; the double-close is harmless —
	// net.Listener.Close returns an error we ignore on the second call).
	_ = socksLnClose(socksLn)
	_ = socksDoneWait(socksDone)
	return nil
}

// socksLnClose closes a possibly-nil listener.
func socksLnClose(ln net.Listener) error {
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// socksDoneWait blocks on a possibly-nil done channel. Returns nil if the
// channel is nil (no SOCKS5 goroutine to wait for).
func socksDoneWait(done <-chan error) error {
	if done == nil {
		return nil
	}
	return <-done
}

// socksDoneChan returns the done channel for select-use, or nil if there is
// no SOCKS5 goroutine (selecting on a nil channel blocks forever, which is
// exactly what we want — the "SOCKS5 exited" branch is unreachable).
func socksDoneChan(done <-chan error) <-chan error {
	return done
}
