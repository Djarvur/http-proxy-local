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

// shutdownTimeout is the budget for gracefully shutting down the HTTP servers
// on context cancellation. Matches the design D2 coordination budget.
const shutdownTimeout = 10 * time.Second

// serveAll runs up to three listeners concurrently and blocks until ctx is
// cancelled OR any one listener exits. On exit it ensures all running
// listeners are closed and HTTP servers shut down.
//
// Listeners:
//   - srv on httpLn: the dumb HTTP/PAC listener (always present).
//   - socksLn (when non-nil): the SOCKS5 listener (opt-in via socks5.listen).
//   - smartSrv on smartLn (when both non-nil): the smart HTTP listener
//     (opt-in via smart.listen).
//
// Coordination contract (design D2): any listener failing unexpectedly tears
// down the others. A half-broken process is worse than a clean exit — silent
// degradation is hard to diagnose; surfacing the failure forces a restart and
// a log line.
//
// Returns the error from whichever listener failed first (nil on clean
// shutdown via ctx cancellation). http.ErrServerClosed is the expected return
// for the normal-shutdown path and is NOT treated as an error by callers.
func serveAll(
	ctx context.Context,
	srv *http.Server,
	httpLn net.Listener,
	socksLn net.Listener,
	smartSrv *http.Server,
	smartLn net.Listener,
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

	var smartDone chan error
	if smartSrv != nil && smartLn != nil {
		smartDone = make(chan error, 1)
		go func() { smartDone <- smartSrv.Serve(smartLn) }()
	}

	// Wait for shutdown trigger: ctx cancellation, or any listener exiting.
	select {
	case <-ctx.Done():
		return shutdownAll(srv, smartSrv, socksLn, smartLn, httpDone, smartDone, socksDone)

	case err := <-httpDone:
		log.Printf("http listener exited: %v", err)
		_ = socksLnClose(socksLn)
		_ = smartLnClose(smartLn)
		_ = shutdownSrv(smartSrv)
		_ = socksDoneWait(socksDone)
		_ = socksDoneWait(smartDone)
		return err

	case <-socksDoneChan(socksDone):
		log.Printf("socks5 listener exited")
		_ = shutdownBothHTTP(ctx, srv, smartSrv, httpDone, smartDone)
		_ = smartLnClose(smartLn)
		return errors.New("socks5 listener exited unexpectedly")

	case err := <-smartDoneChan(smartDone):
		log.Printf("smart listener exited: %v", err)
		_ = shutdownBothHTTP(ctx, srv, smartSrv, httpDone, smartDone)
		_ = socksLnClose(socksLn)
		_ = socksDoneWait(socksDone)
		return err
	}
}

// shutdownBothHTTP shuts down both HTTP servers (dumb + smart) and waits for
// their Serve goroutines to exit. Used when a non-HTTP listener fails.
func shutdownBothHTTP(ctx context.Context, srv, smartSrv *http.Server, httpDone, smartDone <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-httpDone
	_ = shutdownSrv(smartSrv)
	_ = socksDoneWait(smartDone)
	_ = ctx // ctx accepted for symmetry/future use; not currently read
	return nil
}

// shutdownSrv shuts down a possibly-nil HTTP server. No-op when nil.
func shutdownSrv(s *http.Server) error {
	if s == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return s.Shutdown(shutdownCtx)
}

// shutdownAll is the clean-shutdown path: shut down both HTTP servers and
// close both opt-in listeners.
func shutdownAll(
	srv *http.Server,
	smartSrv *http.Server,
	socksLn net.Listener,
	smartLn net.Listener,
	httpDone <-chan error,
	smartDone <-chan error,
	socksDone <-chan error,
) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-httpDone
	_ = shutdownSrv(smartSrv)
	_ = socksDoneWait(smartDone)
	// Closing socksLn is what unblocks socks5.Serve's Accept loop (its own
	// ctx-cancel goroutine also closes it; the double-close is harmless —
	// net.Listener.Close returns an error we ignore on the second call).
	_ = socksLnClose(socksLn)
	_ = socksDoneWait(socksDone)
	_ = smartLnClose(smartLn)
	return nil
}

// socksLnClose closes a possibly-nil listener.
func socksLnClose(ln net.Listener) error {
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// smartLnClose closes a possibly-nil smart listener. (Same shape as
// socksLnClose; kept separate for readability at call sites.)
func smartLnClose(ln net.Listener) error {
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// socksDoneWait blocks on a possibly-nil done channel. Returns nil if the
// channel is nil (no goroutine to wait for).
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

// smartDoneChan returns the smart done channel for select-use, or nil if
// there is no smart goroutine.
func smartDoneChan(done <-chan error) <-chan error {
	return done
}
