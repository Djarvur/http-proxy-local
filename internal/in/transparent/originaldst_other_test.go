//go:build !linux

package transparent

import (
	"context"
	"errors"
	"testing"
)

// TestOriginalDstNonLinuxSentinel proves the !linux OriginalDst fails with
// the ErrLinuxRequired sentinel. Compiled only on non-Linux platforms; on
// linux the positive path is exercised by the kernel smoke instead.
func TestOriginalDstNonLinuxSentinel(t *testing.T) {
	_, err := OriginalDst(context.Background(), nil)
	if err == nil {
		t.Fatal("expected the Linux-required sentinel error")
	}
	if !errors.Is(err, ErrLinuxRequired) {
		t.Errorf("error is not ErrLinuxRequired: %v", err)
	}
}
