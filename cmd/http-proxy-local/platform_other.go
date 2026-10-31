//go:build !linux

package main

// transparentSupported reports whether the transparent listener can run on
// this platform: SO_ORIGINAL_DST is a Linux kernel feature, so non-Linux
// builds (macOS) must refuse to start when the block is enabled rather than
// silently ignoring it (design D6).
func transparentSupported() bool { return false }
