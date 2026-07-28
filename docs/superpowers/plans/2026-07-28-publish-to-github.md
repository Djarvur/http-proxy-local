# Publish http-proxy-local to GitHub under MIT with cross-platform releases

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish the existing `http-proxy-local` Go project to a new public GitHub repo (`github.com/Djarvur/http-proxy-local`) under the MIT license, with tag-driven GoReleaser CI that ships release archives for `{linux, darwin, windows} × {amd64, arm64}`.

**Architecture:** Add an MIT LICENSE, a tiny `internal/version` package whose `Version` string is overridden at build time via `-ldflags`, a `--version` flag in `main`, a `.goreleaser.yaml` + `release.yml` GitHub Actions workflow, and a cross-platform README reframe. The repo is created with `gh repo create --public` after local commits land; the first release (`v0.1.0`) is cut by pushing a git tag (user-gated, **not part of this plan's automated steps**).

**Tech Stack:** Go 1.26.5, GoReleaser v2, GitHub Actions, `gh` CLI (authenticated as `onokonem`), MIT license.

## Global Constraints

- **License:** MIT, copyright line `Copyright (c) 2026 Daniel Podolsky` — verbatim in `LICENSE`.
- **Go version:** 1.26.5 (matches `go.mod`); release workflow uses `actions/setup-go` with `go-version-file: go.mod`.
- **Module path:** `github.com/Djarvur/http-proxy-local` (already set in `go.mod` — no import rewriting).
- **Build targets (exact 6):** `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`. All with `CGO_ENABLED=0` (fully static).
- **Lint gate:** `golangci-lint run ./...` must pass with zero findings (project rule). Every Go change must keep this green.
- **Test gate:** `go test ./...` must pass. New code follows the project's TDD convention.
- **Version injection target:** package path `github.com/Djarvur/http-proxy-local/internal/version`, variable `Version` (string), default value `"dev"`.
- **Commit style:** conventional-commits prefixes (`feat:`, `docs:`, `chore:`, `ci:`, `build:`) — match the existing git log.
- **No secrets in commits:** the secrets sweep is already done (commit `41aaace`). Do not introduce real upstream URLs, credentials, or `/Users/...` paths.
- **Tag/release is user-gated:** the plan stops at "repo pushed, infra ready." Do NOT push a `v*` tag without explicit user confirmation in the session.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `LICENSE` | create | MIT license text, root of repo |
| `internal/version/version.go` | create | `var Version = "dev"` — the single build-stampable string |
| `internal/version/version_test.go` | create | locks the `"dev"` default so local builds are distinguishable from release builds |
| `cmd/http-proxy-local/main.go` | modify | add `--version`/`-v` flag short-circuit + print version once at startup |
| `cmd/http-proxy-local/version_flag_test.go` | create | tests the `--version` flag wiring (package `main`, matches `serve_internal_test.go` style) |
| `.goreleaser.yaml` | create | GoReleaser v2 config: 6-target build, archives (.tar.gz / .zip), checksums, changelog |
| `.github/workflows/release.yml` | create | tag-triggered release workflow |
| `.gitignore` | modify | add `/dist/` (GoReleaser output) |
| `README.md` | modify | cross-platform tagline, CI badge, Installation section, fill the placeholder License section |
| `docs/superpowers/plans/2026-07-28-publish-to-github.md` | (this file) | implementation plan |

Each file has one responsibility and stands alone for review. The `internal/version` package is intentionally trivial (one variable) so it can be referenced by both `main.go` and the GoReleaser ldflags without any cross-package coupling.

---

## Task 1: Add the MIT LICENSE

**Files:**
- Create: `LICENSE`

**Interfaces:**
- Produces: a root-level `LICENSE` file referenced by README's License section (Task 8) and carried in release archives by GoReleaser.

- [ ] **Step 1: Create the LICENSE file**

Create `LICENSE` at the repo root with the exact MIT text. Use the year `2026` and the copyright holder `Daniel Podolsky` (matches the git author from `git config user.name`):

```
MIT License

Copyright (c) 2026 Daniel Podolsky

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

- [ ] **Step 2: Verify the file**

Run: `cat LICENSE | head -3`
Expected output:
```
MIT License

Copyright (c) 2026 Daniel Podolsky
```

- [ ] **Step 3: Commit**

```bash
git add LICENSE
git commit -m "docs: add MIT license"
```

---

## Task 2: Add the `internal/version` package (TDD)

This is the build-stampable version string GoReleaser overrides via `-ldflags`. Kept in its own package so `main` and the ldflags path both reference a stable symbol path.

**Files:**
- Create: `internal/version/version.go`
- Test: `internal/version/version_test.go`

**Interfaces:**
- Produces: `version.Version` (a `string` variable, exported). Consumed by `main.go` (Task 3) and overwritten at build time by GoReleaser (Task 4) via `-X github.com/Djarvur/http-proxy-local/internal/version.Version=...`.

- [ ] **Step 1: Write the failing test**

Create `internal/version/version_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/version/`
Expected: FAIL — `undefined: Version` (package has no `version.go` yet).

- [ ] **Step 3: Write the minimal implementation**

Create `internal/version/version.go`:

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/version/`
Expected: PASS — `ok github.com/Djarvur/http-proxy-local/internal/version`.

- [ ] **Step 5: Commit**

```bash
git add internal/version/version.go internal/version/version_test.go
git commit -m "feat(version): add build-stampable version package"
```

---

## Task 3: Wire `--version` flag and startup version log in `main` (TDD)

Add a `--version` / `-v` flag that prints `http-proxy-local <Version>` and exits 0, plus a one-line version print at normal startup so it appears in logs.

**Files:**
- Modify: `cmd/http-proxy-local/main.go`
- Test: `cmd/http-proxy-local/version_flag_test.go`

**Interfaces:**
- Consumes: `version.Version` from Task 2 (`github.com/Djarvur/http-proxy-local/internal/version`).
- Produces: a `--version`/`-v` flag on the binary; a `versionString()` helper covered by the test.

**Design note — what to test and what not to test:** The Go stdlib's `flag.BoolVar` already guarantees that `--version` and `-v` set the bound bool. Re-testing stdlib wiring by extracting package-level mutable state + a reset helper would be testing the language, not our code — and would clutter `main.go` with test-only scaffolding. We test the *one* thing that is ours and that a future edit could break: the **format** of `versionString()` (the public CLI contract). The flag short-circuit itself is verified by the manual binary check in Step 6.

- [ ] **Step 1: Write the failing test**

Create `cmd/http-proxy-local/version_flag_test.go` in package `main` (matching `serve_internal_test.go`, which is also `package main` to test unexported helpers):

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/http-proxy-local/ -run 'TestVersionString_Format'`
Expected: FAIL — `undefined: versionString` (the helper doesn't exist yet).

- [ ] **Step 3: Write the implementation**

Modify `cmd/http-proxy-local/main.go`. Replace the entire file with:

```go
// Package main is the entrypoint for the http-proxy-local binary.
//
// It is deliberately thin: flag parsing, signal handling, and a single call
// into the composition root (Run in wire.go). All business logic lives under
// internal/.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Djarvur/http-proxy-local/internal/version"
)

// versionString returns the line printed by --version: "http-proxy-local <Version>".
func versionString() string {
	return "http-proxy-local " + version.Version
}

func main() {
	// Register all flags on the default FlagSet before parsing, then parse
	// once. --version and -v both bind to showVersion (either one triggers the
	// version short-circuit). The short-circuit happens before any startup
	// work — no config load, no listeners — and exits 0.
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showVersion, "v", false, "print version and exit (shorthand for --version)")
	configPath := flag.String("config", "", "path to config.yaml (defaults to ~/.config/http-proxy-local/config.yaml)")
	flag.Parse()

	if showVersion {
		log.Println(versionString())
		return
	}

	log.Printf("http-proxy-local %s starting", version.Version)
	if err := mainErr(context.Background(), *configPath); err != nil {
		log.Printf("http-proxy-local exited with error: %v", err)
		os.Exit(1)
	}
}

// mainErr is the non-os.Exit body of main so deferred handlers actually run
// (calling os.Exit directly skips defers — caught by gocritic).
func mainErr(parent context.Context, configPath string) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	return Run(ctx, configPath)
}
```

Implementation notes for the implementer:
- `--version` and `-v` are bound to the *same* `showVersion` variable via `flag.BoolVar`. Either flag sets it; the short-circuit checks one variable. This is the idiomatic stdlib pattern for flag aliases — no `Lookup`/`ParseBool` plumbing needed.
- No `strconv` import is required (the earlier draft's `lookupBool` helper is gone).
- The startup line `log.Printf("http-proxy-local %s starting", version.Version)` puts the version in normal-run logs, not just behind `--version`.
- `flag.Parse()` is called exactly once, after all flags are registered. (The original code's single-parse pattern is preserved and extended — we add the two version flags to the same registration block.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/http-proxy-local/ -run 'TestVersionString_Format'`
Expected: PASS.

- [ ] **Step 5: Run the full cmd test suite to catch regressions**

Run: `go test ./cmd/http-proxy-local/`
Expected: PASS — all existing tests (`TestServeBoth_*`, `TestServeAll_*`, e2e, wire) still pass.

- [ ] **Step 6: Run lint on the changed package**

Run: `golangci-lint run ./cmd/http-proxy-local/`
Expected: zero findings. If `goimports` complains, ensure imports are grouped: stdlib first (`context`, `flag`, `log`, `os`, `os/signal`, `syscall`), then a blank line, then the internal `version` import.

- [ ] **Step 7: Manually verify the binary**

Run: `go build -o /tmp/hpl ./cmd/http-proxy-local && /tmp/hpl --version`
Expected output:
```
http-proxy-local dev
```

Run: `/tmp/hpl -v`
Expected: same output. (This confirms the `-v` shorthand wiring end-to-end — the thing we deliberately did not unit-test, since it's stdlib behavior.)

Run: `cd /tmp && /tmp/hpl --version 2>&1 | head -1` then `rm /tmp/hpl` to clean up.

- [ ] **Step 8: Commit**

```bash
git add cmd/http-proxy-local/main.go cmd/http-proxy-local/version_flag_test.go
git commit -m "feat(cli): add --version flag and print version on startup"
```

---

## Task 4: Add `.goreleaser.yaml`

GoReleaser v2 config that builds all 6 targets, produces archives, and generates checksums.

**Files:**
- Create: `.goreleaser.yaml`

**Interfaces:**
- Consumes: `internal/version.Version` (Task 2) — overwritten via ldflags.
- Produces: 6 archives + `checksums.txt` on each release, published to the GitHub Release by the workflow in Task 5.

- [ ] **Step 1: Create `.goreleaser.yaml`**

Create `.goreleaser.yaml` at the repo root:

```yaml
# GoReleaser config for http-proxy-local.
#
# Builds static binaries for 6 targets (linux/darwin/windows x amd64/arm64),
# packages each as an archive (.tar.gz for unix, .zip for windows), and emits
# a checksums.txt. Triggered by .github/workflows/release.yml on `v*` tags.
#
# Validate locally with:    goreleaser check
# Dry-run a full build:     goreleaser release --clean --snapshot
# (snapshot builds into dist/ without publishing — no tag required.)

version: 2

project_name: http-proxy-local

builds:
  - main: ./cmd/http-proxy-local
    binary: http-proxy-local
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w -X github.com/Djarvur/http-proxy-local/internal/version.Version={{.Version}}

archives:
  - format: tar.gz
    # name_template uses title-cased OS + arch for readable archive names:
    # http-proxy-local_0.1.0_Linux_arm64.tar.gz
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_
      {{- title .Os }}_{{ .Arch }}
    format_overrides:
      - goos: windows
        format: zip

checksum:
  name_template: 'checksums.txt'

changelog:
  use: github
  sort: asc

release:
  draft: false
  prerelease: auto
```

Design notes for the implementer:
- `version: 2` is the current GoReleaser config schema (stable since 2024).
- `CGO_ENABLED=0` makes all binaries fully static so they cross-build from a single Linux runner with no C toolchain.
- `{{.Version}}` in ldflags is the tag without the leading `v` (GoReleaser convention) — so tag `v0.1.0` yields `Version=0.1.0`. The `--version` output will read `http-proxy-local 0.1.0`. This is standard and matches how most Go CLIs report version.
- `format_overrides` for windows → `.zip` is the universal convention (Windows users can't always extract `.tar.gz`).
- `release.prerelease: auto` marks `v0.x.x` releases as pre-releases automatically; flip to `false` once you cut `v1.0.0`. (GoReleaser treats `0.x` as pre-release by default with `auto`.)

- [ ] **Step 2: Validate the config**

Run: `goreleaser check`
Expected: `config is valid` (or equivalent success message). If GoReleaser is not installed, install it via `mise install goreleaser@latest` (mise is the project's tool manager) or `go install github.com/goreleaser/goreleaser/v2@latest`, then retry.

- [ ] **Step 3: Add `dist/` to `.gitignore`**

The `.gitignore` currently ends with:
```
# Editor / OS noise
.DS_Store
*.swp
```

Append a new section at the end:

```

# GoReleaser build output (local snapshot builds, CI artifacts).
/dist/
```

- [ ] **Step 4: Commit**

```bash
git add .goreleaser.yaml .gitignore
git commit -m "build: add GoReleaser config for 6-target cross-platform releases"
```

- [ ] **Step 5 (verification, optional but recommended): Local snapshot build**

Run: `goreleaser release --clean --snapshot`
Expected: builds all 6 archives + `checksums.txt` into `dist/`. No publish (snapshot mode). Verify with `ls dist/` — you should see 6 archives and `checksums.txt`. This confirms every GOOS/GOARCH cross-compiles before any tag is pushed.

If any target fails to build, fix the config and re-run before committing. (The commit in Step 4 already landed; amend it if you fix the config: `git add .goreleaser.yaml && git commit --amend --no-edit`.)

---

## Task 5: Add the release workflow

GitHub Actions workflow that runs GoReleaser on tag push.

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `.goreleaser.yaml` (Task 4), `go.mod` (Go version).
- Produces: a published GitHub Release with 6 archives + checksums, when a `v*` tag is pushed.

- [ ] **Step 1: Create `.github/workflows/release.yml`**

```yaml
# Release workflow: on `v*` tag push, GoReleaser builds the 6 cross-platform
# archives and publishes a GitHub Release. Uses the default GITHUB_TOKEN —
# no extra secrets required.
#
# Local validation: `goreleaser check` and `goreleaser release --snapshot`
# (both run before tagging; see the plan).
name: release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write   # GoReleaser creates the Release + uploads assets

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history → GoReleaser changelog across all tags

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Design notes for the implementer:
- `fetch-depth: 0` is required — GoReleaser walks the full git history to compute the changelog between the previous tag and the current one. The default `fetch-depth: 1` would break the changelog and can cause GoReleaser to error.
- `permissions: contents: write` is the only permission needed. GoReleaser uses it to create the Release and upload the 7 assets. Do NOT add `packages: write` or other permissions (YAGNI — no container/package publishing).
- `GITHUB_TOKEN` is the automatic CI token (`${{ secrets.GITHUB_TOKEN }}`), not a personal access token. No secrets setup is required.
- `version: latest` for the action installs the latest GoReleaser at run time. This is the common choice; if reproducibility becomes a concern later, pin to a specific GoReleaser version.

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: add tag-driven release workflow"
```

---

## Task 6: Reframe the README as cross-platform

Targeted edits to `README.md`: cross-platform tagline, CI badge, a new Installation section, and filling the placeholder License section. **Do not rewrite the file** — match the existing voice and comment density.

**Files:**
- Modify: `README.md` (multiple sections)

**Interfaces:**
- Consumes: the LICENSE file (Task 1) for the License section link; the GitHub Releases URL for the Download section.
- Produces: a public-facing README suitable for the repo front page.

- [ ] **Step 1: Update the intro tagline**

In `README.md`, find this paragraph (lines 3-5):

```
A self-contained local HTTP routing proxy for macOS. It serves a smart
[Proxy Auto-Config (PAC)](https://en.wikipedia.org/wiki/Proxy_auto-config)
file that routes Russian-IP traffic directly and sends everything else
```

Replace the first sentence:

```
A self-contained local HTTP routing proxy for macOS, Linux, and Windows. It serves a smart
```

(Only the first sentence changes — "for macOS" → "for macOS, Linux, and Windows". The rest of the paragraph is unchanged.)

- [ ] **Step 2: Add a CI badge below the `# http-proxy-local` title**

Find line 1:
```
# http-proxy-local
```

Replace with:
```
# http-proxy-local

[![CI](https://github.com/Djarvur/http-proxy-local/actions/workflows/pr.yml/badge.svg)](https://github.com/Djarvur/http-proxy-local/actions/workflows/pr.yml)
```

This inserts the badge right under the title, before the intro paragraph. (The blank line between the title and the badge is already there; the badge block adds its own paragraph break before the intro.)

- [ ] **Step 3: Add an Installation section**

Find the `## Quick start` heading (line 45) and the `### Prerequisites` subheading right after it. The new Installation section goes **before** `## Quick start` (after the client-side-routing paragraph that ends `...HTTPS works without certificate shenanigans.`).

Insert this block immediately before `## Quick start`:

```markdown
## Installation

### Download a binary

Pre-built binaries for Linux, macOS, and Windows (amd64 + arm64 each) are
published on the [Releases page](https://github.com/Djarvur/http-proxy-local/releases/latest).
Download the archive for your platform, extract it, and run:

| Platform | Archive |
|---|---|
| Linux amd64 | `http-proxy-local_<version>_Linux_amd64.tar.gz` |
| Linux arm64 | `http-proxy-local_<version>_Linux_arm64.tar.gz` |
| macOS amd64 | `http-proxy-local_<version>_Darwin_amd64.tar.gz` |
| macOS arm64 (Apple Silicon) | `http-proxy-local_<version>_Darwin_arm64.tar.gz` |
| Windows amd64 | `http-proxy-local_<version>_Windows_amd64.zip` |
| Windows arm64 | `http-proxy-local_<version>_Windows_arm64.zip` |

```sh
# Example (macOS arm64):
curl -L https://github.com/Djarvur/http-proxy-local/releases/latest/download/http-proxy-local_<version>_Darwin_arm64.tar.gz | tar xz
./http-proxy-local --config config.yaml
```

macOS binaries are **unsigned**. On first run Gatekeeper will block them —
right-click → Open (or `xattr -d com.apple.quarantine /path/to/binary`) to
allow execution. See Apple's docs on opening apps from unidentified developers.

### Build from source

Requires Go 1.26+ (or use [mise](https://mise.jdx.dev/) — see Quick start):

```sh
go install github.com/Djarvur/http-proxy-local/cmd/http-proxy-local@latest
```

or clone and build:

```sh
git clone https://github.com/Djarvur/http-proxy-local.git
cd http-proxy-local
make build   # produces ./bin/http-proxy-local
```
```

Design notes for the implementer:
- `<version>` in the table and curl example is a literal placeholder the reader replaces with the version they're downloading (e.g. `0.1.0`). This is the clearest convention for a download table; the Releases link goes to `/latest` so the reader sees the actual current version number.
- The "unsigned" macOS note is honest and saves the user a confused Gatekeeper moment. Keep it concise.
- The Build-from-source subsection mirrors what `CONTRIBUTING.md` already documents but in the shortest useful form for a first-time user.

- [ ] **Step 4: Fill the placeholder License section**

Find the existing `## License` section at the end of the file (line 323), which currently reads:

```
## License

(Add license information here.)
```

Replace with:

```
## License

MIT — see [LICENSE](./LICENSE).
```

- [ ] **Step 5: Verify the README renders**

Run: `grep -n '^#' README.md` (list all headings) and visually confirm the new section ordering:
- `# http-proxy-local`
- (badge)
- intro paragraph
- `## Installation`
  - `### Download a binary`
  - `### Build from source`
- `## Routing precedence`
- ... (rest unchanged)
- `## License`

Expected: `## Installation` appears between the intro and `## Quick start`; `## License` body is the one-liner MIT link.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: reframe README as cross-platform, add install + license sections"
```

---

## Task 7: Final verification + push to GitHub

Run the full gate locally, then create the public repo and push `main`. **Do not push a release tag** — that is user-gated and happens after this plan completes.

**Files:**
- (no file changes — verification + repo creation)

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: all packages pass (`ok ...` for each, no FAIL).

- [ ] **Step 2: Run the strict lint gate**

Run: `golangci-lint run ./...`
Expected: zero findings (this is the project's mandatory gate; the PR workflow enforces it).

- [ ] **Step 3: Verify version wiring end-to-end**

Run: `go build -o /tmp/hpl ./cmd/http-proxy-local && /tmp/hpl --version`
Expected: `http-proxy-local dev`

Run: `goreleaser release --clean --snapshot`
Expected: builds 6 archives + `checksums.txt` into `dist/`. Confirm with `ls dist/*.tar.gz dist/*.zip dist/checksums.txt` — 6 archives + 1 checksums file.

- [ ] **Step 4: Verify git state is clean**

Run: `git status`
Expected: `nothing to commit, working tree clean`.

Run: `git log --oneline -8`
Expected: the 6 new commits from Tasks 1-6 on top of the pre-existing history (the spec commit `0da3ae0` and the msk/ams scrub `41aaace` should be visible in the recent log).

- [ ] **Step 5: Create the public repo and push main**

Run:
```bash
gh repo create Djarvur/http-proxy-local --public --source=. --remote=origin --push
```
Expected: creates `github.com/Djarvur/http-proxy-local` as public, adds it as the `origin` remote, and pushes the `main` branch. Output includes the repo URL.

If `gh` reports the repo name is taken or you lack permission, stop and surface the error to the user — do not fall back to a different name.

- [ ] **Step 6: Verify the push**

Run: `git remote -v`
Expected: an `origin` remote pointing at `git@github.com:Djarvur/http-proxy-local.git` (SSH, per the `gh auth` protocol).

Run: `git ls-remote --heads origin`
Expected: lists `main` at the same SHA as the local `main`.

- [ ] **Step 7: Surface the release-ready state to the user**

Stop here. Report to the user:
- The repo is live at `https://github.com/Djarvur/http-proxy-local`.
- `main` is pushed.
- The release infrastructure (`.goreleaser.yaml` + `release.yml`) is in place but dormant — it triggers on a `v*` tag.
- **To cut the first release (`v0.1.0`), run:** `git tag v0.1.0 && git push origin v0.1.0`. This is an outward-facing action and is left for the user to confirm/trigger.

**Do not run the tag/push-tag commands** as part of this plan. The plan ends at "everything ready to tag."

---

## Self-Review Notes

**Spec coverage check** (every spec section → task):
- §1 Licensing (MIT file) → Task 1 ✓
- §2 Version stamping (`internal/version`, `--version`, startup log) → Tasks 2, 3 ✓
- §3 GoReleaser config (`.goreleaser.yaml`, 6 targets, archives, checksums, changelog) → Task 4 ✓
- §4 Release workflow (`release.yml`, tag trigger, `permissions: contents: write`) → Task 5 ✓
- §5 README reframing (tagline, badge, Installation, License) → Task 6 ✓
- §6 Repo creation & push (`gh repo create --public --push`) → Task 7 Step 5 ✓
- §7 First release `v0.1.0` → intentionally excluded (user-gated); Task 7 Step 7 hands off ✓

**Placeholder scan:** none. Every step has the exact code/config/text. The only `<version>` token in README is an intentional reader-replaced placeholder in a download table (documented in the task notes).

**Type/name consistency:** `version.Version` (string) is consistent across Task 2 (definition), Task 3 (`versionString()` consumer), and Task 4 (ldflags `-X ...version.Version=`). In Task 3: `versionString()` is the only symbol referenced by both the test (Step 1) and the implementation (Step 3); `showVersion` is a local in `main()` bound to both `--version` and `-v` via `flag.BoolVar`. No package-level mutable state is introduced — `main.go` stays free of test-only scaffolding.

**Risk note — GoReleaser config syntax:** Task 4's `.goreleaser.yaml` uses the v2 schema (`version: 2` at top). This schema has been stable since GoReleaser 2.0 (2024). The `release.prerelease: auto` field means `v0.x.x` tags publish as GitHub pre-releases by default — the first `v0.1.0` release will be marked pre-release, which is the correct signal for an early version. The implementer validates the config with `goreleaser check` (Task 4 Step 2) and a local `--snapshot` build (Task 4 Step 5) before any tag is pushed, so syntax errors surface locally.

**Risk note — README `<version>` placeholder:** The `<version>` token in the Task 6 download table is an intentional reader-replaced placeholder (e.g. the reader substitutes `0.1.0`), not a plan placeholder. It is documented as such in the task notes. All actual code/config in the plan is complete.
