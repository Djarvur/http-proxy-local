// Package version holds the build-stampable version string.
//
// The default "dev" marks a locally-built binary; release builds override it
// via -ldflags "-X github.com/Djarvur/http-proxy-local/internal/version.Version=<tag>".
package version

import "testing"

// TestVersion_DefaultIsDev locks the contract that an unoverridden build
// reports "dev". This is how a user can tell a `go build` binary apart from
// an official release: local builds say "dev", releases say "v0.1.0".
func TestVersion_DefaultIsDev(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version = %q, want %q (default for non-release builds)", Version, "dev")
	}
}
