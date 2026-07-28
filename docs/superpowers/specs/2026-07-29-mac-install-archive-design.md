# Local install archive for http-proxy-local (mac amd64)

**Date:** 2026-07-29
**Status:** Approved (pending spec review)
**Scope:** Single implementation plan

## Goal

Produce a one-time, **local** `.tar.gz` archive (built on this Mac, hand-carried
to a second Mac) that installs `http-proxy-local` as a launchd-managed
background service on macOS amd64, fully configured with the operator's real
upstream credential and routing rules — runnable via a single `./install.sh`.

## Non-goals (explicitly out of scope)

- **No changes to the public release.** `.goreleaser.yaml`, `release.yml`, and
  the published v0.1.0 GitHub Release are untouched. The credentialed archive
  is never published.
- **No CI / GitHub secret storage.** The credential never touches GitHub's
  servers in any form.
- **No system-proxy automation.** install.sh prints the `networksetup`
  command but does not run it (operator's explicit choice).
- **No multi-OS support.** macOS amd64 only. The script refuses to run
  elsewhere with a clear message.
- **No code signing / notarization.** Same unsigned posture as the public
  release.
- **No `--uninstall` flag.** Uninstall is documented in the script's output
  and header comment, not implemented as a mode (operator's choice).

## Context

The public v0.1.0 release ships cross-platform binaries, but each archive
contains only the binary + LICENSE + README — no config, and no install
automation. The operator wants to install the proxy on a second Mac with
their **real** configuration (credentialed upstream at
`msk.djarvur.net:3129`, blacklist, SOCKS5 + smart listeners) already in
place, running as a persistent background service.

The credential cannot go through CI or onto the public release. The cleanest
path: build the archive locally on this Mac (which has the live config),
hand-carry it to the target Mac. The credential stays on the operator's
machines only.

**Security note (on record):** the built archive contains the real upstream
credential (`user:pass@msk.djarvur.net:3129`). Exposure is limited to: this
Mac, the transfer medium, and the target Mac — dramatically smaller than a
public release. The credential is **not** committed to the repo (the build
script reads it from the live config at build time) and **not** published. If
the archive file leaks, rotate the upstream credential.

## Design

### Archive contents

Built locally by `scripts/build-mac-archive.sh` into `dist/` (gitignored):

| File | Source | Mode | Purpose |
|---|---|---|---|
| `http-proxy-local` | `GOOS=darwin GOARCH=amd64 go build ./cmd/http-proxy-local` | 0755 | the proxy binary (static, CGO disabled) |
| `config.yaml` | copy of `~/.config/http-proxy-local/config.yaml` | 0600 | the operator's real credentialed config |
| `install.sh` | repo root | 0755 | the installer (this design) |
| `README.md` | repo root | 0644 | project docs |
| `LICENSE` | repo root | 0644 | MIT license |

Output: `dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz`

### `install.sh` — the installer

Run on the target Mac after `tar -xzf` of the archive. POSIX-compatible
shell (`#!/bin/sh`), so it runs under macOS's default `/bin/sh` (dash/bash)
with no extra runtime dependency.

**Behavior, in order:**

1. **Preflight checks.** Refuse to proceed unless:
   - `uname -s` is `Darwin` (else: "this installer is for macOS only").
   - `uname -m` is `x86_64` (else: "this installer is for mac amd64 only;
     for Apple Silicon use the darwin_arm64 release").
   - The archive's `http-proxy-local`, `config.yaml`, `install.sh` are
     present in the current directory (else: "run this from inside the
     extracted archive directory").
   Print the detected OS/arch and the install paths, then proceed.

2. **Privilege model.** The script is NOT run as root. It detects when it
   needs root (only for the binary copy to `/usr/local/bin`) and re-execs
   itself with `sudo` for just that step, OR runs the whole thing under
   `sudo` if the user invoked it that way. The binary itself, the config,
   the plist, and the logs all live under the user's home and need no root.
   Concretely: a helper `sudo_if_needed()` wraps only the
   `/usr/local/bin/http-proxy-local` install step.

3. **Place the binary.** `install -m 0755 http-proxy-local /usr/local/bin/http-proxy-local`
   (via `sudo`). `/usr/local/bin` is on the default PATH for both Intel and
   Apple-Silicon Macs. If the dir doesn't exist, create it (sudo).

4. **Place the config.** `install -d -m 0700 ~/.config/http-proxy-local &&
   install -m 0600 config.yaml ~/.config/http-proxy-local/config.yaml`.
   Mode 0600 because it contains the upstream credential.

5. **Write the launchd plist.** Write
   `~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist` (no sudo —
   user-level LaunchAgent). The plist:
   - Label: `com.djarvur.http-proxy-local`
   - ProgramArguments: `[/usr/local/bin/http-proxy-local, --config,
     /Users/<user>/.config/http-proxy-local/config.yaml]` (absolute config
     path — launchd doesn't inherit `$HOME` reliably).
   - RunAtLoad: `true` (starts at login).
   - KeepAlive: `true` (restarts on crash).
   - StandardOutPath / StandardErrorPath:
     `/Users/<user>/Library/Logs/http-proxy-local.log` (created if missing).
   - The script expands `<user>` to `$(id -un)` at write time — no
     templating engine, just shell variable substitution into the plist XML.

6. **Load it.** `launchctl load
   ~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist`. If a prior
   version is loaded, `launchctl unload` it first (idempotent upgrades).

7. **Verify.** Poll `http://127.0.0.1:8080/proxy.pac` for up to 10 seconds
     (using `curl -sf` or a `/dev/tcp` probe); confirm a response. The
     proxy's first run blocks briefly on the geo-data fetch, so the poll
     must tolerate up to ~10s of no-answer before declaring failure.

8. **Print next steps.** The proxy is running but macOS is not pointed at
   it. Print exactly:
   - The `networksetup -setautoproxyurl "Wi-Fi" "http://127.0.0.1:8080/proxy.pac"`
     command (and note: replace `Wi-Fi` with the active service name if
     different; `networksetup -listallnetworkservices` to list them).
   - How to check logs: `tail -f ~/Library/Logs/http-proxy-local.log`.
   - How to restart after config edits: `launchctl unload <plist>; launchctl load <plist>`
     (config is hot-reloaded, but `listen`/`socks5.listen`/`smart.listen`
     changes need a reload).
   - **Uninstall sequence** (documented, not a flag):
     ```
     launchctl unload ~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist
     rm ~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist
     sudo rm /usr/local/bin/http-proxy-local
     rm -rf ~/.config/http-proxy-local
     # optionally clear the system PAC:
     networksetup -setautoproxyurl "Wi-Fi" " "
     ```

**Properties:**
- **Idempotent:** re-running upgrades in place (overwrites binary/config,
  reloads plist). No "already installed" errors.
- **Fail-fast:** every step checks its exit code and aborts with a clear
  message on failure.
- **Readable:** plain shell, commented per section, no clever tricks.

### `scripts/build-mac-archive.sh` — the local builder

Run on this Mac. POSIX-compatible shell. Steps:

1. **Preflight:** verify `~/.config/http-proxy-local/config.yaml` exists
   (else: "no live config found — nothing to bake in"). Verify `go` is on
   PATH.
2. **Build the binary:** `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build
   -trimpath -o dist/archive-build/http-proxy-local ./cmd/http-proxy-local`.
   Cross-compiled from this arm64 Mac (CGO disabled → static → clean
   cross-compile, no C toolchain needed).
3. **Stage files** into `dist/archive-build/`:
   - `cp ~/.config/http-proxy-local/config.yaml dist/archive-build/config.yaml`
   - `cp install.sh README.md LICENSE dist/archive-build/`
   - `chmod 0755 dist/archive-build/install.sh`
4. **Tar:** `tar -czf dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz -C dist/archive-build .`
5. **Print:** the archive path + size, and the one-line handoff instruction:
   "copy this to your other Mac, extract, run `./install.sh`".

`dist/` is already gitignored. The builder reads the credential from the
live config at build time; the credential is never written to the repo.

### File changes

| File | Action | Shipped in archive? |
|---|---|---|
| `install.sh` | new — the installer | yes |
| `scripts/build-mac-archive.sh` | new — the local builder | no (repo-only) |
| `docs/superpowers/specs/2026-07-29-mac-install-archive-design.md` | new — this spec | no |

No existing files change. `.gitignore` already covers `dist/`.

### Build / verification sequence

This is a one-time local artifact, not a shipped product feature — the
verification bar is "confident it works on the target Mac," not "full CI
coverage." Two tiers:

**On this Mac (build machine, arm64):**

1. `sh -n install.sh scripts/build-mac-archive.sh` — POSIX syntax check
   (mandatory; catches the most common shell bugs).
2. `shellcheck install.sh scripts/build-mac-archive.sh` if available
   (recommended; catches quoting/subtle POSIX issues). Skip gracefully if
   not installed.
3. Run `scripts/build-mac-archive.sh` — produces the archive in `dist/`,
   prints the path.
4. Inspect: `tar -tzf dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz` shows
   the 5 expected files (`http-proxy-local`, `config.yaml`, `install.sh`,
   `README.md`, `LICENSE`).
5. Verify the built binary's arch: `file dist/archive-build/http-proxy-local`
   → must report `Mach-O 64-bit executable x86_64`.

**Full runtime test happens on the target Mac (amd64) — not here.** This
build Mac is arm64, so install.sh's `uname -m == x86_64` check fails by
design. Do NOT hack around this (e.g., relaxing the arch check) to force a
local test — that would test a different script than ships. The syntax +
build + arch checks above give high confidence; the real validation is
running `./install.sh` on the target. The handoff instruction the build
script prints makes this the obvious next step.

## Risks

- **Credential exposure if the archive file leaks.** Mitigation: archive is
  local-only, never published, never committed. If it leaks, rotate the
  upstream credential. Documented in the Security note above.
- **launchd plist path brittleness** (absolute paths, `$HOME` not
  inherited). Mitigation: script expands `$(id -un)` into absolute paths at
  write time; plist is re-generated on each install so it's always correct
  for the current user.
- **`/usr/local/bin` may not exist on a fresh Mac** (it usually does, but
  not guaranteed). Mitigation: `install -d` creates it if missing.
- **`networksetup` service name varies** (`Wi-Fi` vs `Ethernet` vs
  localized names). Mitigation: install.sh prints the command but doesn't
  run it, and tells the user how to list services. The operator picks the
  right one.
- **First-run geo fetch blocks the proxy startup** (the binary blocks on
  the initial RIPE fetch before serving). Mitigation: install.sh's verify
  poll allows 10s; if the fetch is slow the poll may time out and print a
  "still starting, check logs" message rather than declaring success.
  Acceptable — the proxy will come up shortly; the operator checks logs.
