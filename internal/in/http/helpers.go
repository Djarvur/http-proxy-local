package httpserver

import (
	"crypto/tls"
	"encoding/base64"
	"net"
	"time"
)

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

// base64StdEncoding returns standard base64 of s.
func base64StdEncoding(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// touch time to keep the import alive for future timeouts.
var _ = time.Second
