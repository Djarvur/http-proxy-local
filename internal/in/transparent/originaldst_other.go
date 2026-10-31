//go:build !linux

package transparent

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// ErrLinuxRequired is the sentinel returned when the transparent listener's
// destination recovery is used on a non-Linux platform: SO_ORIGINAL_DST is a
// Linux kernel feature (netfilter connection tracking), with no macOS/BSD
// equivalent.
var ErrLinuxRequired = errors.New("transparent interception requires Linux (SO_ORIGINAL_DST)")

// OriginalDst always fails on non-Linux builds. The composition root refuses
// to START the listener when transparent.listen is set on a non-Linux
// platform (wire.go platform guard); this symbol exists so the package
// compiles everywhere and the failure contract is testable per-platform.
func OriginalDst(_ context.Context, conn net.Conn) (string, error) {
	return "", fmt.Errorf("%w: %T", ErrLinuxRequired, conn)
}
