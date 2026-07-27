package socks5_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/in/socks5"
)

// TestNegotiateMethod exercises the RFC 1928 greeting handshake with pure
// io.Reader/io.Writer — no net.Listener needed. The function accepts only
// method 0x00 (No Authentication Required); any greeting that doesn't offer
// 0x00 gets 0xFF (No Acceptable Methods) and an error.
func TestNegotiateMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     []byte // bytes the client sends
		wantReply []byte // bytes we expect to write back (nil if none)
		wantErr   bool
	}{
		{
			name:      "offers no-auth (0x00) — accept",
			input:     []byte{0x05, 0x01, 0x00}, // VER=5, NMETHODS=1, METHOD=0x00
			wantReply: []byte{0x05, 0x00},       // VER=5, METHOD=0x00
			wantErr:   false,
		},
		{
			name:      "offers no-auth among multiple methods — accept",
			input:     []byte{0x05, 0x02, 0x02, 0x00}, // VER=5, NMETHODS=2, [userpass, no-auth]
			wantReply: []byte{0x05, 0x00},
			wantErr:   false,
		},
		{
			name:      "offers only user/pass (0x02) — reject with 0xFF",
			input:     []byte{0x05, 0x01, 0x02},
			wantReply: []byte{0x05, 0xFF},
			wantErr:   true,
		},
		{
			name:      "offers only GSSAPI (0x01) — reject with 0xFF",
			input:     []byte{0x05, 0x01, 0x01},
			wantReply: []byte{0x05, 0xFF},
			wantErr:   true,
		},
		{
			// Wrong SOCKS version (e.g. SOCKS4 client connecting). RFC 1928
			// doesn't define a method-selection reply for this; we close
			// without writing, matching the spec scenario "wrong SOCKS
			// version rejected".
			name:      "wrong version (0x04) — no reply, error",
			input:     []byte{0x04, 0x01, 0x00},
			wantReply: nil,
			wantErr:   true,
		},
		{
			// Truncated greeting: declared NMETHODS=3 but only 1 method byte.
			name:      "truncated method list — no reply, error",
			input:     []byte{0x05, 0x03, 0x00}, // VER=5, NMETHODS=3, but only 1 byte
			wantReply: nil,
			wantErr:   true,
		},
		{
			// Empty input (connection closed before any greeting).
			name:      "empty input — no reply, error",
			input:     []byte{},
			wantReply: nil,
			wantErr:   true,
		},
		{
			// Only the version byte — NMETHODS missing.
			name:      "only version byte — no reply, error",
			input:     []byte{0x05},
			wantReply: nil,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := bytes.NewReader(tt.input)
			var w bytes.Buffer

			err := socks5.NegotiateMethod(r, &w)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// On success or non-version errors we may still have written bytes.
			got := w.Bytes()
			if tt.wantReply != nil {
				if !bytes.Equal(got, tt.wantReply) {
					t.Errorf("reply = % x, want % x", got, tt.wantReply)
				}
			} else if len(got) != 0 {
				t.Errorf("expected no reply bytes, got % x", got)
			}
		})
	}
}

// TestNegotiateMethod_UnreadableWriter guards the write path: if writing the
// reply fails, the error MUST propagate.
func TestNegotiateMethod_UnreadableWriter(t *testing.T) {
	t.Parallel()
	r := bytes.NewReader([]byte{0x05, 0x01, 0x00})
	w := &failingWriter{err: errors.New("disk full")}
	if err := socks5.NegotiateMethod(r, w); err == nil {
		t.Fatal("expected write error to propagate")
	}
}

type failingWriter struct{ err error }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// Compile-time: ensure failingWriter satisfies io.Writer.
var _ io.Writer = (*failingWriter)(nil)
