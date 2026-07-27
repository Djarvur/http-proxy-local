package socks5

import (
	"fmt"
	"io"
)

// WriteReply writes a SOCKS5 reply with the given REP code. The BND.ADDR and
// BND.PORT fields are always 0.0.0.0:0 (design D5):
//
//   - On success (REP=0x00): we don't bind anything locally — the cascade
//     tunnels through the upstream — so reporting the bind address is
//     meaningless. RFC 1928 permits this and every tested SOCKS5 client
//     (Telegram desktop/Android, the Go SOCKS5 client) ignores the field.
//   - On failure (any non-zero REP): BND.ADDR/BND.PORT are undefined by the
//     RFC; zeroing them is the conventional choice.
//
// The emitted frame is exactly 10 bytes:
//
//	VER REP RSV ATYP BND.ADDR(4) BND.PORT(2)
//	05  ??  00  01  00 00 00 00  00 00
func WriteReply(w io.Writer, code byte) error {
	// Pre-allocated to avoid a heap allocation per reply — the reply is
	// emitted on every connection and the byte layout is fixed.
	var frame [10]byte
	frame[0] = protoVersion
	frame[1] = code
	frame[2] = 0x00 // reserved
	frame[3] = atypIPv4
	// frame[4..7] = 0.0.0.0 (already zero)
	// frame[8..9] = port 0  (already zero)
	if _, err := w.Write(frame[:]); err != nil {
		return fmt.Errorf("write socks5 reply: %w", err)
	}
	return nil
}
