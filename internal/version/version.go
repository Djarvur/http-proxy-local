// Package version holds the build-stampable version string.
//
// Version defaults to "dev" for local `go build` builds. Release builds
// override it at compile time via ldflags, e.g.:
//
//	go build -ldflags "-X github.com/Djarvur/http-proxy-local/internal/version.Version=v0.1.0"
//
// GoReleaser injects the git tag automatically — see .goreleaser.yaml.
package version

// Version is the build version. "dev" for local builds; the release tag
// (e.g. "v0.1.0") for official builds.
var Version = "dev"
