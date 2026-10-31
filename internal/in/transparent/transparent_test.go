// In-package tests for the transparent listener: they construct Server
// literals with fake seams (origDst, resolve, dialer, upstream) — the seams
// are unexported by design, production wiring uses NewServer. See change
// add-transparent-listener, specs/transparent-listener.
package transparent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/dialchain"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// cfgFor builds a minimal config with the smart tuning the chain reads.
func cfgFor() port.Config {
	return port.Config{
		Listen: "127.0.0.1:8080",
		Smart: port.Smart{
			DialTimeout:      time.Second,
			DialAttempts:     1,
			NegativeCacheTTL: time.Minute,
		},
	}
}

func cfgPtrOf(cfg *port.Config) *atomic.Pointer[port.Config] {
	p := &atomic.Pointer[port.Config]{}
	p.Store(cfg)
	return p
}

func rangesPtrOf(r *port.RURanges) *atomic.Pointer[port.RURanges] {
	p := &atomic.Pointer[port.RURanges]{}
	if r != nil {
		p.Store(r)
	}
	return p
}

// staticResolve returns fixed IPs for any host.
func staticResolve(ips ...net.IP) dialchain.ResolveFunc {
	return func(context.Context, string) ([]net.IP, error) { return ips, nil }
}

// staticOrigDst returns an origDst always reporting the given target.
func staticOrigDst(target string) func(context.Context, net.Conn) (string, error) {
	return func(context.Context, net.Conn) (string, error) { return target, nil }
}

// drainedPipe returns a net.Conn whose peer is drained in the background, so
// writes into it never block.
func drainedPipe(t *testing.T) net.Conn {
	t.Helper()
	c1, c2 := net.Pipe()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := c2.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = c2.Close() })
	return c1
}

// syncBuf is a race-free log sink for tests: Server.logf writes from
// per-connection goroutines while the test reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Printf appends one formatted line.
func (s *syncBuf) Printf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(&s.b, format, args...)
	s.b.WriteByte('\n')
}

// String returns everything logged so far.
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHandleConn_OrigDstFailureClosesConn proves a getsockopt failure closes
// the client connection (spec: Recover the original destination / failure).
func TestHandleConn_OrigDstFailureClosesConn(t *testing.T) {
	t.Parallel()

	cfg := cfgFor()
	srv := &Server{
		cfgPtr: cfgPtrOf(&cfg),
		origDst: func(context.Context, net.Conn) (string, error) {
			return "", errors.New("no original destination (not redirected?)")
		},
		negCache: dialchain.NewNegativeCache(),
	}

	c1, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()
	done := make(chan struct{})
	go func() {
		srv.HandleConn(context.Background(), c1)
		close(done)
	}()

	// The handler closes c1 → the peer's read fails.
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Error("expected client conn close (read error), got a successful read")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHandleConn_RoutesByOriginalDst proves the recovered destination drives
// the chain (default → Upstream with the ORIGINAL target) and the relay is
// bidirectional raw TCP.
func TestHandleConn_RoutesByOriginalDst(t *testing.T) {
	t.Parallel()

	const target = "93.184.216.34:443" // non-RU → default [Upstream]
	cfg := cfgFor()

	upDial := make(chan string, 1)
	outboundDone := make(chan struct{})
	upstream := func(target string) (net.Conn, error) {
		upDial <- target
		c1, c2 := net.Pipe()
		go func() {
			defer close(outboundDone)
			defer func() { _ = c2.Close() }()
			buf := make([]byte, 5)
			if _, err := c2.Read(buf); err != nil {
				return
			}
			_, _ = c2.Write([]byte("pong!"))
		}()
		return c1, nil
	}

	srv := &Server{
		cfgPtr:    cfgPtrOf(&cfg),
		rangesPtr: rangesPtrOf(nil),
		origDst:   staticOrigDst(target),
		resolve:   staticResolve(),
		dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("direct must not be dialed")
		},
		upstream: upstream,
		negCache: dialchain.NewNegativeCache(),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go srv.HandleConn(context.Background(), conn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("ping!")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read relayed reply: %v", err)
	}
	if string(buf) != "pong!" {
		t.Errorf("relayed %q, want %q", buf, "pong!")
	}

	select {
	case got := <-upDial:
		if got != target {
			t.Errorf("upstream dialed %q, want the original destination %q", got, target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not dialed")
	}
	select {
	case <-outboundDone:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound side not finished")
	}
}

// TestHandleConn_DirectWarns proves a Direct-capable strategy logs a warning
// naming the destination, and the connection still proceeds.
func TestHandleConn_DirectWarns(t *testing.T) {
	t.Parallel()

	logBuf := &syncBuf{}
	cfg := cfgFor()
	ranges := &port.RURanges{Ranges: []port.Range{{Low: ipRange(5, 6, 7, 0), High: ipRange(5, 6, 7, 255)}}}

	srv := &Server{
		cfgPtr:    cfgPtrOf(&cfg),
		rangesPtr: rangesPtrOf(ranges),
		origDst:   staticOrigDst("5.6.7.8:443"),
		resolve:   staticResolve(net.IPv4(5, 6, 7, 8).To4()),
		dialer: func(context.Context, string, string) (net.Conn, error) {
			return drainedPipe(t), nil
		},
		upstream: func(string) (net.Conn, error) {
			return nil, errors.New("upstream must not be dialed for RU")
		},
		negCache: dialchain.NewNegativeCache(),
		logf:     logBuf.Printf,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		go srv.HandleConn(context.Background(), conn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "WARNING routing resolves to Direct") &&
			strings.Contains(logBuf.String(), "5.6.7.8:443") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no Direct-loop warning naming the destination; log:\n%s", logBuf.String())
}

// TestHandleConn_UpstreamOnlyDoesNotWarn proves default targets produce no
// loop warning.
func TestHandleConn_UpstreamOnlyDoesNotWarn(t *testing.T) {
	t.Parallel()

	logBuf := &syncBuf{}
	cfg := cfgFor()

	srv := &Server{
		cfgPtr:    cfgPtrOf(&cfg),
		rangesPtr: rangesPtrOf(nil),
		origDst:   staticOrigDst("1.2.3.4:443"),
		resolve:   staticResolve(),
		dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("direct must not be dialed")
		},
		upstream: func(string) (net.Conn, error) { return drainedPipe(t), nil },
		negCache: dialchain.NewNegativeCache(),
		logf:     logBuf.Printf,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		go srv.HandleConn(context.Background(), conn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = client.Write([]byte("x"))

	time.Sleep(300 * time.Millisecond)
	if strings.Contains(logBuf.String(), "WARNING routing resolves to Direct") {
		t.Errorf("loop warning emitted for Upstream-only routing:\n%s", logBuf.String())
	}
}

// TestDarwinBuildGate proves a darwin/amd64 cross-build compiles (the
// build-tag split stays coherent). Compile-only, no execution.
func TestDarwinBuildGate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("darwin cross-build gate only meaningful from linux")
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=amd64", "GOCACHE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("GOOS=darwin go build: %v\n%s", err, out)
	}
}

// ipRange packs an IPv4 into the uint32 range form.
func ipRange(a, b, c, d byte) uint32 {
	return uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d)
}
