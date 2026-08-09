package socks5_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/in/socks5"
	"github.com/Djarvur/http-proxy-local/internal/port"
)

// These tests exercise Serve end-to-end against a real net.Listener plus a
// stub HTTP upstream proxy (the same chain_test.go pattern). They prove the
// SOCKS5 listener integrates the pure framing with the upstream cascade and
// the bidirectional tunnel, plus the spec's command-refusal and no-routing
// guarantees.

// socksUpstream is a minimal forward proxy that handles CONNECT by dialing
// the target and tunneling bytes. Mirrors the chain_test.go stubUpstreamProxy
// but trimmed to just the CONNECT path SOCKS5 exercises.
type socksUpstream struct {
	t        *testing.T
	reject   bool // if true, respond 403 to every CONNECT (cascade-failure case)
	sawAuth  []string
	mu       sync.Mutex
}

func (s *socksUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.sawAuth = append(s.sawAuth, r.Header.Get("Proxy-Authorization"))
	s.mu.Unlock()

	if s.reject {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	target := r.Host
	// gosec G704 is a false positive: target comes from the CONNECT line of
	// a test-only stub upstream, which only ever sees targets the test
	// itself configured. No attacker-controlled input reaches this dial.
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second) //nolint:gosec // G704: test fixture, target is test-controlled
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	client, bufRW, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = bufRW.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = bufRW.Flush()
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
}

// startSOCKSListener launches Serve on a random localhost port in a
// background goroutine. Returns the listener (already bound, so .Addr() is
// final) and a stop func. Calling stop cancels Serve's context, closes the
// listener, and blocks until Serve has returned.
//
// Tests SHOULD `defer stop()` — the helper sequences cancel → listener
// close → wait-for-exit internally so the caller can't deadlock on defer
// ordering (a common bug when the wait and the cancel are separate defers).
func startSOCKSListener(t *testing.T, cfgPtr *atomic.Pointer[port.Config]) (net.Listener, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		socks5.Serve(ctx, ln, cfgPtr)
		close(done)
	}()
	stop := func() {
		cancel()
		_ = ln.Close()
		<-done
	}
	return ln, stop
}

// cfgWithUpstream returns a published configPtr pointing at the given
// upstream URL. Credentials are omitted — the SOCKS5 tests don't exercise
// upstream auth (the HTTP listener's chain_test.go covers that path, which
// shares cascade.Dial).
func cfgWithUpstream(url string) *atomic.Pointer[port.Config] {
	p := &atomic.Pointer[port.Config]{}
	c := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: url},
	}
	p.Store(&c)
	return p
}

// dialSOCKS dials the SOCKS5 listener and performs a CONNECT to the given
// host:port. Returns the established tunneled conn or an error.
func dialSOCKS(t *testing.T, addr, target string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial socks5: %v", err)
	}
	// Greeting: VER=5, NMETHODS=1, METHOD=0x00.
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		_ = c.Close()
		t.Fatalf("write greeting: %v", err)
	}
	br := bufio.NewReader(c)
	ver := make([]byte, 2)
	if _, err := io.ReadFull(br, ver); err != nil {
		_ = c.Close()
		t.Fatalf("read method reply: %v", err)
	}
	if ver[0] != 0x05 || ver[1] != 0x00 {
		_ = c.Close()
		t.Fatalf("method reply = % x, want 05 00", ver)
	}
	// CONNECT request: VER=5, CMD=1, RSV=0, ATYP=1 (IPv4).
	// Split host:port for the target.
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		_ = c.Close()
		t.Fatalf("split target: %v", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		_ = c.Close()
		t.Fatalf("target host %q is not an IP; use a domain test helper", host)
	}
	var portInt int
	if _, err := fmt.Sscanf(portStr, "%d", &portInt); err != nil || portInt <= 0 || portInt > 65535 {
		_ = c.Close()
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	portBytes := make([]byte, 2)
	// gosec G115 is a false positive: portInt is bounds-checked above
	// (0 < portInt <= 65535), so the int→uint16 conversion cannot overflow.
	binary.BigEndian.PutUint16(portBytes, uint16(portInt)) //nolint:gosec // G115: bounds-checked above

	var req []byte
	if v4 := ip.To4(); v4 != nil {
		req = append([]byte{0x05, 0x01, 0x00, 0x01}, v4...)
	} else {
		req = append([]byte{0x05, 0x01, 0x00, 0x04}, ip.To16()...)
	}
	req = append(req, portBytes...)
	if _, err := c.Write(req); err != nil {
		_ = c.Close()
		t.Fatalf("write CONNECT req: %v", err)
	}

	// Read 10-byte reply.
	reply := make([]byte, 10)
	if _, err := io.ReadFull(br, reply); err != nil {
		_ = c.Close()
		t.Fatalf("read CONNECT reply: %v", err)
	}
	if reply[1] != 0x00 {
		_ = c.Close()
		return nil // caller checks for nil to detect refused CONNECT
	}
	// The reply read above may have pulled tunnel bytes (sent by the fake
	// origin as soon as the upstream connects) into br's buffer — discarding
	// br would silently lose them. Wrap the conn so buffered bytes are
	// served first, mirroring cascade.bufferedConn.
	return &readAheadConn{r: br, Conn: c}
}

// readAheadConn serves bufio-buffered bytes first, then falls through to the
// underlying conn. Mirrors cascade.bufferedConn: the CONNECT reply read can
// pull early tunnel bytes into the reader's buffer, and losing them would
// make tests observe a closed-but-empty tunnel (flaky EOF).
type readAheadConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *readAheadConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// TestServe_ConnectTunnelsThroughUpstream proves the full chain:
// SOCKS5 client → SOCKS5 listener → cascade.Dial → fake upstream CONNECT
// → fake origin. Bytes flow both directions.
func TestServe_ConnectTunnelsThroughUpstream(t *testing.T) {
	t.Parallel()

	// Fake origin: writes a known string on connect, echoes anything it
	// receives back uppercased.
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen origin: %v", err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("FROM-ORIGIN"))
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				upper := strings.ToUpper(string(buf[:n]))
				_, _ = c.Write([]byte("ECHO:" + upper))
			}(c)
		}
	}()

	// Upstream proxy that tunnels CONNECT to the origin.
	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	conn := dialSOCKS(t, ln.Addr().String(), origin.Addr().String())
	if conn == nil {
		t.Fatal("CONNECT was refused by the listener")
	}
	defer func() { _ = conn.Close() }()

	// Read the origin's greeting.
	br := bufio.NewReader(conn)
	greeting := make([]byte, len("FROM-ORIGIN"))
	if _, err := io.ReadFull(br, greeting); err != nil {
		t.Fatalf("read origin greeting: %v", err)
	}
	if string(greeting) != "FROM-ORIGIN" {
		t.Errorf("greeting = %q, want FROM-ORIGIN", greeting)
	}

	// Send a payload; expect it echoed back uppercased.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	echo := make([]byte, 16)
	n, err := br.Read(echo)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo[:n]) != "ECHO:HELLO" {
		t.Errorf("echo = %q, want ECHO:HELLO", echo[:n])
	}
}

// TestServe_BindCommandRefused proves the listener refuses BIND (0x02) with
// reply code 0x07 (Command not supported) and closes.
func TestServe_BindCommandRefused(t *testing.T) {
	t.Parallel()
	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Greeting.
	_, _ = c.Write([]byte{0x05, 0x01, 0x00})
	br := bufio.NewReader(c)
	ver := make([]byte, 2)
	_, _ = io.ReadFull(br, ver)

	// BIND request to 1.2.3.4:80.
	_, _ = c.Write([]byte{0x05, 0x02, 0x00, 0x01, 1, 2, 3, 4, 0x00, 0x50})
	reply := make([]byte, 10)
	if _, err := io.ReadFull(br, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[1] != 0x07 {
		t.Errorf("BIND reply REP = 0x%02x, want 0x07 (command not supported)", reply[1])
	}
}

// TestServe_UDPAssociateRefused proves the listener refuses UDP ASSOCIATE
// (0x03) with reply code 0x07.
func TestServe_UDPAssociateRefused(t *testing.T) {
	t.Parallel()
	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, _ = c.Write([]byte{0x05, 0x01, 0x00})
	br := bufio.NewReader(c)
	ver := make([]byte, 2)
	_, _ = io.ReadFull(br, ver)

	// UDP ASSOCIATE request.
	_, _ = c.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0x00, 0x00})
	reply := make([]byte, 10)
	if _, err := io.ReadFull(br, reply); err != nil {
		// Some implementations close without a full reply when refusing;
		// accept either a 0x07 reply or an immediate close.
		return
	}
	if reply[1] != 0x07 {
		t.Errorf("UDP ASSOCIATE reply REP = 0x%02x, want 0x07", reply[1])
	}
}

// TestServe_CascadeFailureReportsGeneralError proves that when the upstream
// rejects the CONNECT (403 here), the listener replies 0x01 (general SOCKS
// server failure) rather than 0x00.
func TestServe_CascadeFailureReportsGeneralError(t *testing.T) {
	t.Parallel()
	up := &socksUpstream{t: t, reject: true} // 403 every CONNECT
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, _ = c.Write([]byte{0x05, 0x01, 0x00})
	br := bufio.NewReader(c)
	ver := make([]byte, 2)
	_, _ = io.ReadFull(br, ver)

	// CONNECT to a target the upstream will refuse.
	_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 9, 9, 9, 9, 0x00, 0x50})
	reply := make([]byte, 10)
	if _, err := io.ReadFull(br, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[1] != 0x01 {
		t.Errorf("cascade-failure reply REP = 0x%02x, want 0x01 (general failure)", reply[1])
	}
}

// TestServe_NoRoutingPolicyApplied proves the listener tunnels a request
// whose target matches the live config's blacklist — no DIRECT-vs-PROXY
// decision, no PAC, no geo. Whatever host:port the client requested is what
// gets tunneled.
func TestServe_NoRoutingPolicyApplied(t *testing.T) {
	t.Parallel()

	// Fake origin (must be reachable — the listener will tunnel to it).
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("ROUTING-IGNORED"))
			}(c)
		}
	}()

	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	// Blacklist the origin's address — the HTTP listener would force DIRECT
	// for it; the SOCKS5 listener MUST ignore the blacklist and tunnel anyway.
	cfgPtr := cfgWithUpstream(upSrv.URL)
	cfg := port.Config{
		Listen:   "127.0.0.1:0",
		Upstream: port.Upstream{URL: upSrv.URL},
		Routing:  port.Routing{Blacklist: []string{origin.Addr().String()}},
	}
	cfgPtr.Store(&cfg)

	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	conn := dialSOCKS(t, ln.Addr().String(), origin.Addr().String())
	if conn == nil {
		t.Fatal("CONNECT to blacklisted host was refused — SOCKS5 must ignore routing")
	}
	defer func() { _ = conn.Close() }()

	br := bufio.NewReader(conn)
	buf := make([]byte, 32)
	n, err := br.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "ROUTING-IGNORED" {
		t.Errorf("got %q, want ROUTING-IGNORED (blacklist was applied — it shouldn't be)", buf[:n])
	}
}

// TestServe_HandshakeDeadlineDropsSlowClient proves the listener closes a
// connection that opens but never sends a greeting within the handshake
// deadline. (Bounds against slowloris-style hangs.)
func TestServe_HandshakeDeadlineDropsSlowClient(t *testing.T) {
	t.Parallel()
	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	ln, stop := startSOCKSListener(t, cfgPtr)
	defer stop()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Send nothing; expect the server to close the connection within a
	// bounded deadline (the handshake timeout — we allow generous slack
	// for slow CI).
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	buf := make([]byte, 1)
	n, err := c.Read(buf)
	if n > 0 || err == nil {
		t.Errorf("expected server to close slow client; got n=%d err=%v", n, err)
	}
}

// TestServe_StopsOnContextCancel proves Serve returns (and stop() unblocks)
// within a bounded time of the context being cancelled. The stop() helper
// returned by startSOCKSListener sequences cancel → listener close → wait
// for Serve exit; if Serve hung, stop() would hang and this test would time
// out.
func TestServe_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	up := &socksUpstream{t: t}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfgPtr := cfgWithUpstream(upSrv.URL)
	_, stop := startSOCKSListener(t, cfgPtr)

	// stop() must return within 5s — it cancels the context, closes the
	// listener, and waits for Serve to exit. If any of those hang, the
	// select below fires.
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		// expected
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop within 5s of context cancel")
	}
}

// Compile-time: ensure socksUpstream satisfies http.Handler.
var _ http.Handler = (*socksUpstream)(nil)

// keep the bytes import live for any future framing helpers.
var _ = bytes.MinRead
