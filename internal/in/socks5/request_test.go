package socks5_test

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/in/socks5"
)

// TestReadRequest exercises the SOCKS5 request parser for all three address
// types plus the malformed-input cases. The function returns the raw command
// byte; the caller (Serve) decides whether to accept CONNECT and refuse
// BIND/UDP. IPv6 hosts are formatted as [addr]:port per RFC 3986 so they can
// be passed verbatim to net.JoinHostPort-friendly dialers.
func TestReadRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantCmd  byte
		wantHost string
		wantPort uint16
		wantErr  bool
	}{
		{
			name: "CONNECT IPv4",
			// VER=5, CMD=1 (CONNECT), RSV=0, ATYP=1 (IPv4),
			// 1.2.3.4, port=443 (0x01BB)
			input:    append([]byte{0x05, 0x01, 0x00, 0x01, 1, 2, 3, 4}, bePort(443)...),
			wantCmd:  0x01,
			wantHost: "1.2.3.4:443",
			wantPort: 443,
		},
		{
			name: "CONNECT domain",
			// VER=5, CMD=1, RSV=0, ATYP=3 (domain), len=11, "example.com", port=443
			input: append(
				append([]byte{0x05, 0x01, 0x00, 0x03, 11}, []byte("example.com")...),
				bePort(443)...,
			),
			wantCmd:  0x01,
			wantHost: "example.com:443",
			wantPort: 443,
		},
		{
			name: "CONNECT IPv6",
			// VER=5, CMD=1, RSV=0, ATYP=4 (IPv6), 2001:db8::1, port=443
			input: append(
				append([]byte{0x05, 0x01, 0x00, 0x04}, ipv6Bytes(t, "2001:db8::1")...),
				bePort(443)...,
			),
			wantCmd:  0x01,
			wantHost: "[2001:db8::1]:443",
			wantPort: 443,
		},
		{
			name: "BIND request returns cmd 0x02 (caller refuses)",
			input: append(
				[]byte{0x05, 0x02, 0x00, 0x01, 5, 6, 7, 8},
				bePort(1080)...,
			),
			wantCmd:  0x02,
			wantHost: "5.6.7.8:1080",
			wantPort: 1080,
		},
		{
			name: "UDP ASSOCIATE request returns cmd 0x03 (caller refuses)",
			input: append(
				[]byte{0x05, 0x03, 0x00, 0x01, 9, 10, 11, 12},
				bePort(1080)...,
			),
			wantCmd:  0x03,
			wantHost: "9.10.11.12:1080",
			wantPort: 1080,
		},
		{
			name:    "wrong version rejected",
			input:   []byte{0x04, 0x01, 0x00, 0x01, 1, 2, 3, 4, 0x01, 0xBB},
			wantErr: true,
		},
		{
			name:    "truncated header (fewer than 4 bytes)",
			input:   []byte{0x05, 0x01, 0x00},
			wantErr: true,
		},
		{
			name:    "IPv4 ATYP but fewer than 4 address bytes",
			input:   []byte{0x05, 0x01, 0x00, 0x01, 1, 2}, // only 2 of 4 octets
			wantErr: true,
		},
		{
			name:    "domain ATYP with length but no name bytes",
			input:   []byte{0x05, 0x01, 0x00, 0x03, 11}, // declared len=11, no bytes
			wantErr: true,
		},
		{
			name:    "IPv6 ATYP but fewer than 16 address bytes",
			input:   append([]byte{0x05, 0x01, 0x00, 0x04}, make([]byte, 8)...), // 8 of 16
			wantErr: true,
		},
		{
			name:     "missing port bytes after address",
			input:    []byte{0x05, 0x01, 0x00, 0x01, 1, 2, 3, 4}, // no port
			wantErr:  true,
		},
		{
			name:    "unknown ATYP rejected",
			input:   []byte{0x05, 0x01, 0x00, 0x09, 1, 2, 3, 4, 0x01, 0xBB},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := bytes.NewReader(tt.input)
			cmd, host, port, err := socks5.ReadRequest(r)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd != tt.wantCmd {
				t.Errorf("cmd = 0x%02x, want 0x%02x", cmd, tt.wantCmd)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if port != tt.wantPort {
				t.Errorf("port = %d, want %d", port, tt.wantPort)
			}
		})
	}
}

// bePort returns the 2-byte big-endian encoding of a port.
func bePort(p uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, p)
	return b
}

// ipv6Bytes returns the 16-byte representation of an IPv6 address string.
func ipv6Bytes(t *testing.T, s string) []byte {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("parse %q: nil", s)
	}
	v6 := ip.To16()
	if v6 == nil {
		t.Fatalf("to16 %q: nil", s)
	}
	return v6
}
