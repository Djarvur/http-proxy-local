//go:build linux

package transparent

import (
	"context"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// OriginalDst recovers the pre-redirect destination of a connection the
// kernel redirected to this listener (iptables REDIRECT / -j DNAT in nat
// OUTPUT or PREROUTING). It reads SO_ORIGINAL_DST (SOL_IP, option 80) off the
// accepted socket — Linux-only by kernel contract.
//
// The conn must be a *net.TCPConn (the listener is TCP); its raw fd is
// obtained via SyscallConn without blocking the runtime poller.
func OriginalDst(_ context.Context, conn net.Conn) (string, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return "", fmt.Errorf("transparent: original dst requires *net.TCPConn, got %T", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("transparent: syscall conn: %w", err)
	}
	var (
		sin   sockaddrIn
		errno unix.Errno
	)
	ctrlErr := raw.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(sin))
		// SOL_IP=0, SO_ORIGINAL_DST=80. The kernel writes a sockaddr_in for
		// IPv4 redirects — the deployment this project targets. The IPv6
		// TPROXY variant (IP6T_SO_ORIGINAL_DST) is out of scope.
		//nolint:gosec // G103: getsockopt requires unsafe pointers by syscall contract; buffer is stack-local.
		_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_IP, 80,
			uintptr(unsafe.Pointer(&sin)), uintptr(unsafe.Pointer(&l)), 0)
	})
	if ctrlErr != nil {
		return "", fmt.Errorf("transparent: control: %w", ctrlErr)
	}
	if errno != 0 {
		return "", fmt.Errorf("transparent: getsockopt(SO_ORIGINAL_DST): %w", errno)
	}
	return sin.hostPort()
}

// sockaddrIn mirrors struct sockaddr_in (netinet/in.h) for the getsockopt
// buffer. All fields are network byte order.
type sockaddrIn struct {
	Family uint16
	Port   uint16
	Addr   [4]byte
	Zero   [8]byte
}

// hostPort formats the recovered sockaddr as host:port (IPv6 bracketing is
// moot here — this is the IPv4 path; kept uniform via JoinHostPort).
func (s *sockaddrIn) hostPort() (string, error) {
	if s.Family != uint16(unix.AF_INET) {
		return "", fmt.Errorf("transparent: unexpected original-dst family %d", s.Family)
	}
	ip := net.IPv4(s.Addr[0], s.Addr[1], s.Addr[2], s.Addr[3])
	// Port arrives in network byte order.
	port := s.Port>>8 | s.Port<<8
	return net.JoinHostPort(ip.String(), fmt.Sprint(port)), nil
}
