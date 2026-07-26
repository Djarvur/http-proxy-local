// Package cascade is the shared upstream-connection layer for inbound adapters.
// It establishes a tunneled TCP connection to a target host:port by dialing
// the configured upstream proxy and issuing an HTTP CONNECT to it. When no
// upstream is configured, it dials the target directly.
//
// Both inbound adapters (HTTP/PAC and SOCKS5) use this package so the
// subtle tunneling, half-close, and buffered-bytes-after-read behaviors live
// in exactly one place.
package cascade

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// connectTimeout bounds the dial+CONNECT handshake to the upstream.
const connectTimeout = 30 * time.Second

// Dial establishes a tunneled TCP connection to target (host:port) by dialing
// the configured upstream and issuing a CONNECT to it. If u.URL is empty, it
// dials the target directly (mirroring the direct-mode semantics of the
// upstream-configured case). Optional upstream basic auth is applied only
// when both Username and Password are non-empty.
//
// The returned net.Conn already accounts for any bytes the CONNECT response
// read left in its bufio buffer — callers read transparently.
func Dial(u port.Upstream, target string) (net.Conn, error) {
	if u.URL == "" {
		// No upstream configured — connect directly. (Useful for local testing.)
		return net.DialTimeout("tcp", target, connectTimeout)
	}

	parsed, err := url.Parse(u.URL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream url: %w", err)
	}

	conn, err := dialProxy(parsed)
	if err != nil {
		return nil, fmt.Errorf("dial upstream: %w", err)
	}

	// Send CONNECT.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.Username != "" && u.Password != "" {
		req += "Proxy-Authorization: Basic " + basicAuth(u.Username, u.Password) + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	// Read response status line + headers.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream CONNECT returned %d", resp.StatusCode)
	}

	// If bufio cached any bytes from the response body, they're already part
	// of the tunnel — wrap the conn so reads see them first.
	if br.Buffered() > 0 {
		return &bufferedConn{r: br, Conn: conn}, nil
	}
	return conn, nil
}

// Tunnel copies bytes bidirectionally between two connections, returning when
// both directions complete. When one direction finishes (half-close), we
// half-close the write side of the peer so the peer's read loop can flush
// any remaining bytes; we do NOT set a deadline that would abort an
// in-flight read on the other side (a common bug that drops the last
// packet of a tunnel).
func Tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		// Half-close: signal to b that a has no more to write. If b is a
		// *net.TCPConn this lets b's peer finish reading; CloseWrite is
		// a no-op for non-TCPConns (e.g. tls.Conn).
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
}

// dialProxy dials the proxy host with the appropriate transport (TLS for
// https upstream URLs, plain TCP otherwise).
func dialProxy(u *url.URL) (net.Conn, error) {
	host := u.Host
	if u.Scheme == "https" && !strings.Contains(host, ":") {
		host += ":443"
	} else if u.Scheme == "http" && !strings.Contains(host, ":") {
		host += ":80"
	}
	if u.Scheme == "https" {
		return tlsDial("tcp", host)
	}
	return net.DialTimeout("tcp", host, connectTimeout)
}

// tlsDial wraps net.DialTimeout + crypto/tls handshake for https upstream
// proxies. The server certificate is verified against the system roots.
func tlsDial(network, addr string) (net.Conn, error) {
	raw, err := net.DialTimeout(network, addr, connectTimeout)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{ServerName: ""}) // SNI derived from addr below
	// Pull SNI host from addr (strip port).
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		tlsConn = tls.Client(raw, &tls.Config{ServerName: host})
	}
	if err := tlsConn.Handshake(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return tlsConn, nil
}

// basicAuth returns the base64-encoded "user:pass" string for the
// Proxy-Authorization header.
func basicAuth(user, pass string) string {
	return base64StdEncoding(user + ":" + pass)
}

// bufferedConn wraps a net.Conn but serves buffered bytes first from a
// bufio.Reader. Used when CONNECT's response read left tunnel bytes in the
// reader's buffer.
type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
