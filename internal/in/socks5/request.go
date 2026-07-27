package socks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// SOCKS5 address types (ATYP field in the request).
const (
	atypIPv4   byte = 0x01
	atypDomain byte = 0x03
	atypIPv6   byte = 0x04
)

// ReadRequest parses a SOCKS5 request from r (VER, CMD, RSV, ATYP, DST.ADDR,
// DST.PORT). It returns the command byte, the destination formatted as
// host:port (with IPv6 hosts bracketed per RFC 3986), the numeric port, and
// any error.
//
// The function returns the raw command byte for ANY recognized command
// (CONNECT, BIND, UDP ASSOCIATE); the caller decides whether to accept
// CONNECT and refuse the others. This keeps the pure parser free of policy
// and the refusal logic testable at the Serve level.
//
// No DNS resolution is performed — the domain name is returned verbatim so
// the upstream cascade's CONNECT can resolve it. This is deliberate: the
// cascade hands host:port to the configured upstream proxy, which does its
// own resolution.
func ReadRequest(r io.Reader) (cmd byte, host string, port uint16, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, "", 0, fmt.Errorf("read request header: %w", err)
	}
	if hdr[0] != protoVersion {
		return 0, "", 0, fmt.Errorf("unsupported SOCKS version 0x%02x", hdr[0])
	}
	cmd = hdr[1]
	// hdr[2] is the reserved byte (RSV), which MUST be 0x00 per RFC 1928.
	// We don't reject on a non-zero RSV — staying liberal costs nothing and
	// some real-world clients emit non-zero reserved bytes.

	host, err = readAddr(r, hdr[3])
	if err != nil {
		return 0, "", 0, err
	}

	var portBytes [2]byte
	if _, err := io.ReadFull(r, portBytes[:]); err != nil {
		return 0, "", 0, fmt.Errorf("read port: %w", err)
	}
	port = binary.BigEndian.Uint16(portBytes[:])

	// Format host:port with IPv6 bracketed so the cascade can use it
	// directly (e.g. net.JoinHostPort would produce the same form).
	host = joinHostPort(host, hdr[3], port)
	return cmd, host, port, nil
}

// readAddr reads the DST.ADDR field per ATYP. For ATYP=domain the name is
// returned verbatim (no DNS); for ATYP=IPv4/IPv6 the address is rendered to
// its canonical textual form.
func readAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		var raw [4]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return "", fmt.Errorf("read IPv4 address: %w", err)
		}
		return net.IP(raw[:]).String(), nil
	case atypIPv6:
		var raw [16]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return "", fmt.Errorf("read IPv6 address: %w", err)
		}
		return net.IP(raw[:]).String(), nil
	case atypDomain:
		var lenByte [1]byte
		if _, err := io.ReadFull(r, lenByte[:]); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		name := make([]byte, int(lenByte[0]))
		if _, err := io.ReadFull(r, name); err != nil {
			return "", fmt.Errorf("read domain name: %w", err)
		}
		return string(name), nil
	default:
		return "", fmt.Errorf("unsupported ATYP 0x%02x", atyp)
	}
}

// joinHostPort formats host:port with IPv6 hosts bracketed per RFC 3986.
// atyp is consulted to distinguish IPv6 (which needs brackets) from
// IPv4/domain (which don't).
func joinHostPort(host string, atyp byte, port uint16) string {
	if atyp == atypIPv6 {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}
