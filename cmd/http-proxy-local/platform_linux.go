//go:build linux

package main

// transparentSupported reports whether the transparent listener can run on
// this platform: SO_ORIGINAL_DST is a Linux kernel feature.
func transparentSupported() bool { return true }
