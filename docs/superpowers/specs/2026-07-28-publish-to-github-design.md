# Publish http-proxy-local to GitHub under MIT with cross-platform releases

**Date:** 2026-07-28
**Status:** Approved (pending spec review)
**Scope:** Single implementation plan

## Goal

Publish the existing `http-proxy-local` Go project to a new public GitHub
repository (`github.com/Djarvur/http-proxy-local`) under the MIT license, and
add tag-driven GoReleaser CI that publishes release artifacts for the six
targets `{linux, darwin, windows} × {amd64, arm64}`.

## Non-goals (explicitly out of scope)

- Homebrew tap, Scoop manifest, Chocolatey package — add later on demand.
- SBOM generation — can layer on after v0.1.0.
- macOS code signing / notarization — unsigned is acceptable for an MIT CLI.
- Docker image — not requested.
- No behavior changes to the proxy itself.

## Context

The project is a working Go 1.26.5 CLI at module path
`github.com/Djarvur/http-proxy-local`. It already has:

- `pr.yml` (test + strict lint + coverage floor) and `periodic.yml` (weekly
  health) GitHub Actions workflows.
- README, CONTRIBUTING, `docs/architecture.md`, `config.example.yaml`.
- Makefile + `mise.toml` for tool pinning.

It does **not** have: a git remote, a LICENSE file, a release workflow, any
release artifacts, or version stamping. The README is currently framed as
"for macOS" only.

`gh` CLI is authenticated as `onokonem` (the GitHub account that owns
`Djarvur`). Module path already matches the target repo, so no import
rewriting is needed.

## Design

### 1. Licensing

- Add `LICENSE` at repo root — standard MIT text, copyright line
  `Copyright (c) 2026 Daniel Podolsky`.
- Add a `## License` section to README pointing to the LICENSE file.
- GoReleaser metadata carries MIT as the project license (informational).

### 2. Version stamping

- New package `internal/version/version.go` with `var Version = "dev"`.
  Local `go build` yields `"dev"`; release builds override it via `-ldflags`.
- Add a `--version` / `-v` flag to `cmd/http-proxy-local/main.go` that prints
  `http-proxy-local <Version>` and exits 0.
- Print the version once at startup so it appears in logs
  (`http-proxy-local v0.1.0 starting…`), keeping the existing log style.
- The flag is wired through `mainErr`/`Run` only insofar as a `--version`
  short-circuit happens in `main` before the composition root is called —
  no business-logic signature changes.

Testing is split across the two packages:

- `internal/version/version_test.go` — asserts the default value is
  `"dev"` (locks in the contract that local builds report `dev`).
- `cmd/http-proxy-local/version_flag_test.go` (package `main`, matching
  the existing `serve_internal_test.go` pattern) — parses `--version` /
  `-v` via `flag.CommandLine` in a subtest and asserts the process exits
  0 with the expected stdout, using the same helpers/style as the
  package's other tests.

### 3. GoReleaser config (`.goreleaser.yaml`)

Builds all six targets in one `builds` entry. CGO disabled so binaries are
fully static and cross-build cleanly from a single `ubuntu-latest` runner.

```yaml
version: 2
project_name: http-proxy-local
builds:
  - main: ./cmd/http-proxy-local
    binary: http-proxy-local
    env: [CGO_ENABLED=0]
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/Djarvur/http-proxy-local/internal/version.Version={{.Version}}
archives:
  - format: tar.gz
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{- title .Os }}_{{ .Arch }}
    format_overrides:
      - goos: windows
        format: zip
checksum:
  name_template: 'checksums.txt'
changelog:
  use: github
  sort: asc
```

Artifacts per release (6 archives + 1 checksums file):

- `http-proxy-local_<v>_Linux_amd64.tar.gz`
- `http-proxy-local_<v>_Linux_arm64.tar.gz`
- `http-proxy-local_<v>_Darwin_amd64.tar.gz`
- `http-proxy-local_<v>_Darwin_arm64.tar.gz`
- `http-proxy-local_<v>_Windows_amd64.zip`
- `http-proxy-local_<v>_Windows_arm64.zip`
- `checksums.txt`

No `release.github` block is needed — GoReleaser publishes to the repo it
runs in by default.

### 4. Release workflow (`.github/workflows/release.yml`)

```yaml
name: release
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  goreleaser:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

`fetch-depth: 0` is required so GoReleaser can compute a changelog across
the full history. Uses the default `GITHUB_TOKEN` — no extra secrets. The
existing `pr.yml` and `periodic.yml` are untouched.

### 5. README reframing (cross-platform)

Targeted edits only — not a rewrite:

- **Header tagline:** "for macOS" → "for macOS, Linux, and Windows".
- **CI badge:** add a `pr` workflow status badge at the top.
- **New `## Installation` section** (after the intro/diagram, before
  `## Routing precedence`) containing:
  - **Download** subsection: link to `/releases/latest` and a 6-row table
    of archive → target, with a one-line extract-and-run instruction.
  - **Build from source** subsection: `go install
    github.com/Djarvur/http-proxy-local/cmd/http-proxy-local@latest`.
- **Proxy configuration pointers:** one short paragraph per OS describing
  where to point the OS/browser at the PAC URL — macOS Network Settings,
  Linux env vars or PAC, Windows Internet Options. Lightweight.
- **New `## License` section** at the bottom: MIT, link to LICENSE file.

The existing diagram stays (it still illustrates the concept); only its
caption is generalized.

### 6. Repo creation & push

After local commits land on `main`:

```
gh repo create Djarvur/http-proxy-local --public --source=. --remote=origin --push
```

This creates the public repo, adds it as `origin`, and pushes `main` in one
step. `git push -u origin main` is implicit.

### 7. First release (`v0.1.0`)

**Not executed as part of the implementation.** Tagging is an outward-facing
action; the plan stops at "everything is ready to tag." When the user
confirms, the release is cut with:

```
git tag v0.1.0
git push origin v0.1.0
```

Pushing the tag triggers `release.yml`, which builds the 6 archives +
checksums and publishes the GitHub Release with an auto-generated changelog.

## File changes

| File | Action |
|---|---|
| `LICENSE` | new — MIT text |
| `internal/version/version.go` | new — `var Version = "dev"` |
| `internal/version/version_test.go` | new — locks default value `"dev"` |
| `cmd/http-proxy-local/version_flag_test.go` | new — covers `--version` flag (package `main`) |
| `cmd/http-proxy-local/main.go` | edit — add `--version`/`-v`, print version on startup |
| `.goreleaser.yaml` | new |
| `.github/workflows/release.yml` | new |
| `README.md` | edit — cross-platform reframe, Installation, License, badges |
| `.gitignore` | edit — add `dist/` (GoReleaser output) |

## Build / verification sequence

1. `go test ./...` — existing suite + new version test stays green.
2. `golangci-lint run ./...` — zero findings (project gate).
3. `go build -o /tmp/hpl ./cmd/http-proxy-local && /tmp/hpl --version` —
   prints `http-proxy-local dev`, exits 0.
4. `goreleaser release --clean --snapshot` (local, no tag) — builds all 6
   archives + checksums into `dist/` without publishing; confirms the
   config is valid and every target compiles.
5. `gh repo create … --push` — repo goes live, `main` is pushed.
6. (User-gated) `git tag v0.1.0 && git push origin v0.1.0` — triggers the
   release workflow; verify the GitHub Release page shows 7 artifacts.

## Risks

- **Tag pushes a Release to a public GitHub repo.** Mitigation: the tag
  step is user-gated and explicitly called out in the plan; the snapshot
  build verifies everything before any tag exists.
- **First-time GoReleaser config issues** (typo in template, missing
  field). Mitigation: `goreleaser check` and `--snapshot` run locally
  before pushing the tag.
- **`gh repo create --public` is irreversible-ish** (deletion is possible
  but the name may be squatted). Mitigation: confirmed with the user up
  front; module path already matches.
- **README edits could drift from project voice.** Mitigation: targeted
  edits only, match existing comment density and structure.
