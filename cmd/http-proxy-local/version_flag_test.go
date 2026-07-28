package main

import (
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/version"
)

// TestVersionString_Format locks the exact line the --version flag prints:
// "http-proxy-local <Version>". The binary name and the version are both part
// of the public CLI contract — a change here is breaking. The value of
// version.Version is "dev" in test builds; the format check is value-agnostic.
func TestVersionString_Format(t *testing.T) {
	t.Parallel()

	got := versionString()
	want := "http-proxy-local " + version.Version
	if got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}
}
