package socks5_test

import (
	"bytes"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/in/socks5"
)

// TestWriteReply locks in the exact bytes the listener sends for each reply
// code. Per design D5, the success reply always uses BND.ADDR=0.0.0.0:0 —
// we don't bind anything locally (the cascade tunnels through the upstream),
// and every tested SOCKS5 client ignores the field. The reply is 10 bytes:
// VER, REP, RSV, ATYP=1 (IPv4), 4 bytes 0.0.0.0, 2 bytes port=0.
func TestWriteReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code byte
		want []byte
	}{
		{
			name: "success (0x00)",
			code: 0x00,
			want: []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0},
		},
		{
			name: "general failure (0x01)",
			code: 0x01,
			want: []byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0},
		},
		{
			name: "command not supported (0x07) — BIND/UDP refusal",
			code: 0x07,
			want: []byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var w bytes.Buffer
			if err := socks5.WriteReply(&w, tt.code); err != nil {
				t.Fatalf("WriteReply: %v", err)
			}
			got := w.Bytes()
			if len(got) != len(tt.want) {
				t.Fatalf("reply length = %d, want %d; bytes=% x", len(got), len(tt.want), got)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("reply = % x, want % x", got, tt.want)
			}
		})
	}
}
