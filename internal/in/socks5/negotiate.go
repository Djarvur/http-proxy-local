package socks5

import (
	"fmt"
	"io"
)

// RFC 1928 protocol constants. Constants are added per-phase as the
// corresponding framing function is implemented; pre-declaring all of them
// trips golangci-lint's unused check.
const (
	protoVersion byte = 0x05

	// Methods.
	methodNoAuth       byte = 0x00
	methodNoAcceptable byte = 0xFF
)

// NegotiateMethod performs the RFC 1928 method-selection handshake on r/w.
//
// It reads the client greeting (VER, NMETHODS, METHODS...) and accepts the
// "No Authentication Required" method (0x00) when offered. When 0x00 is
// offered it writes the method-selection reply [0x05, 0x00] and returns nil.
//
// When 0x00 is NOT among the offered methods, it writes [0x05, 0xFF] (No
// Acceptable Methods) and returns a non-nil error so the caller closes the
// connection.
//
// When the client's version byte is not 0x05, or the greeting is truncated
// (fewer bytes than declared NMETHODS, or fewer than 2 bytes total), it
// returns a non-nil error WITHOUT writing a reply — RFC 1928 doesn't define
// a method-selection reply for malformed greetings, and writing one could
// confuse a non-SOCKS5 client. The caller closes the connection.
//
// The function is pure over io.Reader/io.Writer; it has no net.Listener or
// deadline dependency, which keeps it byte-level testable per design D6.
func NegotiateMethod(r io.Reader, w io.Writer) error {
	// Version + NMETHODS: 2 bytes.
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("read greeting header: %w", err)
	}
	if hdr[0] != protoVersion {
		// Wrong SOCKS version — don't write a method-selection reply (RFC
		// 1928 only defines it for VER=5; writing to a SOCKS4 client would
		// be garbage).
		return fmt.Errorf("unsupported SOCKS version 0x%02x", hdr[0])
	}
	nMethods := int(hdr[1])
	if nMethods == 0 {
		// Client declared zero methods — nothing acceptable by definition.
		// Reply 0xFF per RFC 1928 §3, then return error so caller closes.
		_, _ = w.Write([]byte{protoVersion, methodNoAcceptable})
		return fmt.Errorf("client offered zero methods")
	}

	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(r, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	for _, m := range methods {
		if m == methodNoAuth {
			if _, err := w.Write([]byte{protoVersion, methodNoAuth}); err != nil {
				return fmt.Errorf("write method reply: %w", err)
			}
			return nil
		}
	}

	// No acceptable method offered. Reply 0xFF and let the caller close.
	if _, err := w.Write([]byte{protoVersion, methodNoAcceptable}); err != nil {
		return fmt.Errorf("write no-acceptable-methods reply: %w", err)
	}
	return fmt.Errorf("no acceptable authentication method offered")
}
