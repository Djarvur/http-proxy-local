# Local mac install archive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce a local `.tar.gz` archive (built on this Mac, hand-carried to a second Mac) that installs `http-proxy-local` as a launchd-managed background service on macOS amd64, fully configured with the operator's real credentialed config, via a single `./install.sh`.

**Architecture:** Two shell scripts. `install.sh` (shipped in the archive) places the binary in `/usr/local/bin`, the config in `~/.config/http-proxy-local/`, writes a user-level launchd plist that auto-starts at login with KeepAlive, loads it, polls the PAC endpoint to confirm the proxy is up, and prints the manual `networksetup` step. `scripts/build-mac-archive.sh` (repo-only) cross-compiles the amd64 binary, stages it with the operator's live config + install.sh + README + LICENSE, and tars it into `dist/`. The credential is read from the live config at build time — never committed, never published.

**Tech Stack:** POSIX shell (`/bin/sh`), `go build` cross-compile, `launchctl`, `tar`. No Go code changes. No CI changes.

## Global Constraints

- **POSIX shell only.** Both scripts use `#!/bin/sh`. No bashisms (no `[[ ]]`, no arrays, no `$(<file)`, no `local` outside functions where dash disallows — actually `local` IS supported by dash inside functions, so it's fine; avoid `${var,,}`, process substitution `<()`, etc.). macOS `/bin/sh` is dash-compatible.
- **macOS amd64 target only.** install.sh refuses to run on non-Darwin or non-x86_64 with a clear message. Do not add multi-OS logic.
- **No public-release changes.** `.goreleaser.yaml`, `release.yml`, the published v0.1.0 Release are untouched. The archive is local-only.
- **Credential handling.** The credential lives in `~/.config/http-proxy-local/config.yaml` (the operator's live config). `scripts/build-mac-archive.sh` reads it at build time into the archive. The credential is NEVER committed to the repo and NEVER published. install.sh installs config.yaml with mode 0600.
- **Binary block on startup.** The proxy blocks on its initial RIPE geo-data fetch (up to 60s) before binding the listener (see `cmd/http-proxy-local/wire.go:144-146`, `waitForFirstPAC`). install.sh's verification poll MUST allow up to 30s for the proxy to start serving `/proxy.pac`.
- **No sudo for the whole script.** install.sh runs unprivileged; it re-execs with `sudo` ONLY for the `/usr/local/bin` binary placement step (and `/usr/local/bin` creation if missing). Config, plist, logs all live under `$HOME` and need no root.
- **No system-proxy automation.** install.sh prints the `networksetup -setautoproxyurl` command but does not run it.
- **Uninstall is documented, not a flag.** No `--uninstall` mode; the uninstall command sequence appears in install.sh's final output and header comment.
- **Commit style:** conventional-commits prefixes (`feat:`, `chore:`, `docs:`) — match the existing git log.
- **`dist/` is already gitignored** (added during the publish-to-github work). Do not change `.gitignore`.

---

## File Structure

| File | Action | Shipped in archive? | Responsibility |
|---|---|---|---|
| `install.sh` | create | yes (mode 0755) | The installer: preflight → place binary → place config → write+load plist → verify → print next steps |
| `scripts/build-mac-archive.sh` | create | no (repo-only) | Cross-compile amd64 binary, stage live config + install.sh + README + LICENSE, tar into `dist/` |
| (no existing files change) | — | — | `.goreleaser.yaml`, `release.yml`, all Go code untouched |

`install.sh` is the archive's entry point — everything the target Mac needs to do is in it. `scripts/build-mac-archive.sh` is the build-time assembler that only runs on this Mac. The two never depend on each other at runtime; the only coupling is that the builder copies `install.sh` into the archive.

---

## Task 1: Write `install.sh` (with `sh -n` validation)

The installer. This is the core deliverable — it's what runs on the target Mac. TDD doesn't apply (shell script, no test framework in the project), so the validation gate is `sh -n` (syntax) + a dry structural review against the spec's behavior list. The full runtime test happens on the target Mac (this arm64 build Mac correctly fails the arch check).

**Files:**
- Create: `install.sh`

**Interfaces:**
- Consumes: the archive's `http-proxy-local` binary and `config.yaml` (sitting next to install.sh in the extracted archive dir). Nothing from earlier tasks.
- Produces: an installed, running proxy on the target Mac. No exports for later tasks.

- [ ] **Step 1: Write `install.sh`**

Create `install.sh` at the repo root with this exact content:

```sh
#!/bin/sh
# install.sh — install http-proxy-local as a launchd service on macOS amd64.
#
# Run this from inside the extracted archive directory (the dir containing
# http-proxy-local, config.yaml, and this script). It will:
#   1. Place the binary at /usr/local/bin/http-proxy-local  (needs sudo)
#   2. Place the config at ~/.config/http-proxy-local/config.yaml  (mode 0600)
#   3. Write a user-level launchd plist that auto-starts at login + KeepAlive
#   4. Load the plist (starts the proxy now, no re-login needed)
#   5. Poll http://127.0.0.1:8080/proxy.pac to confirm the proxy is up
#   6. Print the manual step to point macOS at the PAC (NOT run automatically)
#
# Idempotent: re-running upgrades in place (overwrites binary/config, reloads
# the plist). Fails fast on any error.
#
# This installer does NOT touch macOS system proxy settings. The final output
# shows the networksetup command to run manually.
#
# --- Uninstall ---
#   launchctl unload ~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist
#   rm ~/Library/LaunchAgents/com.djarvur.http-proxy-local.plist
#   sudo rm /usr/local/bin/http-proxy-local
#   rm -rf ~/.config/http-proxy-local
#   rm -f ~/Library/Logs/http-proxy-local.log
#   # optionally clear the system PAC you set manually:
#   networksetup -setautoproxyurl "Wi-Fi" " "
# ----------------

set -eu

LABEL="com.djarvur.http-proxy-local"
BIN_DST="/usr/local/bin/http-proxy-local"
CONFIG_DIR="$HOME/.config/http-proxy-local"
CONFIG_DST="$CONFIG_DIR/config.yaml"
PLIST_DST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DST="$HOME/Library/Logs/http-proxy-local.log"
PAC_URL="http://127.0.0.1:8080/proxy.pac"
# The proxy blocks on its initial RIPE geo fetch before binding the listener
# (up to 60s in the binary). 30s is enough for a warm geo cache; if the first
# fetch is slow the poll times out and we tell the user to check logs.
STARTUP_TIMEOUT=30

# --- helpers ---

die() {
    echo "install: error: $*" >&2
    exit 1
}

# run a single command via sudo, prompting only if needed
sudo_cmd() {
    sudo "$@"
}

# --- 1. preflight ---

echo "install: http-proxy-local for macOS amd64"
echo

[ "$(uname -s)" = "Darwin" ] || die "this installer is for macOS only (got $(uname -s))."
[ "$(uname -m)" = "x86_64" ] || die "this installer is for mac amd64 only (got $(uname -m)). For Apple Silicon use the darwin_arm64 release."

# The archive's payload must sit next to this script in the CWD.
[ -f ./http-proxy-local ] || die "http-proxy-local binary not found in the current directory. Run this from inside the extracted archive."
[ -f ./config.yaml ]      || die "config.yaml not found in the current directory. Run this from inside the extracted archive."

echo "  binary : $BIN_DST"
echo "  config : $CONFIG_DST"
echo "  plist  : $PLIST_DST"
echo "  log    : $LOG_DST"
echo

# --- 2. place the binary (needs root for /usr/local/bin) ---

if [ ! -d /usr/local/bin ]; then
    echo "install: creating /usr/local/bin (needs sudo)"
    sudo_cmd install -d -m 0755 /usr/local/bin
fi

echo "install: copying binary to $BIN_DST (needs sudo)"
sudo_cmd install -m 0755 ./http-proxy-local "$BIN_DST"

# --- 3. place the config (no root; 0600 because it has the upstream cred) ---

echo "install: copying config to $CONFIG_DST"
install -d -m 0700 "$CONFIG_DIR"
install -m 0600 ./config.yaml "$CONFIG_DST"

# --- 4. write the launchd plist (user-level LaunchAgent, no root) ---

USER_NAME="$(id -un)"

echo "install: writing launchd plist to $PLIST_DST"
install -d -m 0755 "$(dirname "$PLIST_DST")"
install -d -m 0755 "$(dirname "$LOG_DST")"

cat > "$PLIST_DST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${BIN_DST}</string>
        <string>--config</string>
        <string>${CONFIG_DST}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${LOG_DST}</string>
    <key>StandardErrorPath</key>
    <string>${LOG_DST}</string>
</dict>
</plist>
EOF

# --- 5. load the plist (unload first if a prior version is loaded) ---

if launchctl list "$LABEL" >/dev/null 2>&1; then
    echo "install: unloading previous version"
    launchctl unload "$PLIST_DST" || true
fi

echo "install: loading plist (starting the proxy)"
launchctl load "$PLIST_DST"

# --- 6. verify the proxy is serving ---

echo "install: waiting for the proxy to start (up to ${STARTUP_TIMEOUT}s; first run fetches geo data)"
i=0
while [ "$i" -lt "$STARTUP_TIMEOUT" ]; do
    if curl -sf -o /dev/null "$PAC_URL" 2>/dev/null; then
        echo "install: proxy is up — $PAC_URL responds"
        break
    fi
    i=$((i + 1))
    sleep 1
done

if [ "$i" -ge "$STARTUP_TIMEOUT" ]; then
    echo "install: warning: proxy did not respond within ${STARTUP_TIMEOUT}s." >&2
    echo "install: warning: it may still be fetching geo data. Check the log:" >&2
    echo "install: warning:   tail -f $LOG_DST" >&2
fi

# --- 7. print the manual next step (system PAC) ---

echo
echo "================================================================"
echo "install: done. The proxy is running and will auto-start at login."
echo
echo "NEXT STEP — point macOS at the PAC (run manually):"
echo
echo "  networksetup -setautoproxyurl \"Wi-Fi\" \"$PAC_URL\""
echo
echo "  (replace \"Wi-Fi\" with your active service name if different;"
echo "   list them with: networksetup -listallnetworkservices)"
echo
echo "Logs:        tail -f $LOG_DST"
echo "Restart:     launchctl unload $PLIST_DST \\"
echo "             && launchctl load $PLIST_DST"
echo "Uninstall:   see the header comment of this script"
echo "================================================================"
```

Implementation notes for the implementer:
- `set -eu` — fail fast on any error or unset variable. Critical for an installer.
- `die()` writes to stderr and exits non-zero; used for all preflight failures.
- The plist uses absolute paths (`/Users/<name>/...`) because launchd does not reliably inherit `$HOME`. `${USER_NAME}`, `${BIN_DST}`, `${CONFIG_DST}`, `${LOG_DST}` are all expanded by the heredoc at write time — the plist on disk has no shell variables in it.
- `curl -sf -o /dev/null` — `-s` silent, `-f` fail on HTTP error (returns non-zero if the PAC isn't serving yet), `-o /dev/null` discard body. The loop sleeps 1s between tries.
- The `sudo_cmd` helper exists so the two privileged steps (binary placement, possible `/usr/local/bin` creation) are clearly marked and could be audited; the rest of the script runs unprivileged.
- The final `echo` block uses `$PAC_URL` and the absolute paths so the printed instructions are copy-pasteable with no editing (except the `Wi-Fi` service name, which the message calls out).
- **Do not** add a `--uninstall` flag (per spec). The uninstall sequence lives in the header comment and is referenced from the final output.

- [ ] **Step 2: Syntax-check the script**

Run: `sh -n install.sh`
Expected: no output, exit 0. (This is the mandatory gate — catches the most common shell bugs.)

- [ ] **Step 3: Check executable bit**

Run: `ls -l install.sh`
Expected: the file is NOT yet executable (it's a source file in the repo; the builder chmods it when staging into the archive). That's correct — do not `chmod` it here. The repo file is read by `scripts/build-mac-archive.sh`, which sets 0755 on the staged copy.

- [ ] **Step 4: Commit**

```bash
git add install.sh
git commit -m "feat(install): add mac amd64 install.sh (launchd service installer)"
```

---

## Task 2: Write `scripts/build-mac-archive.sh` (with `sh -n` validation)

The local builder. Cross-compiles the amd64 binary, stages the live config + install.sh + README + LICENSE, tars into `dist/`. Reads the credential from the live config at build time.

**Files:**
- Create: `scripts/build-mac-archive.sh`

**Interfaces:**
- Consumes: `install.sh` (Task 1), `README.md`, `LICENSE` (repo root), `~/.config/http-proxy-local/config.yaml` (operator's live config), `./cmd/http-proxy-local` (the Go main package).
- Produces: `dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz` (the hand-off artifact).

- [ ] **Step 1: Create the scripts directory if needed**

Run: `mkdir -p scripts && ls scripts 2>/dev/null || echo "(scripts dir ready)"`

- [ ] **Step 2: Write `scripts/build-mac-archive.sh`**

Create `scripts/build-mac-archive.sh` with this exact content:

```sh
#!/bin/sh
# build-mac-archive.sh — assemble a local, hand-carried install archive for
# http-proxy-local on macOS amd64.
#
# Builds an amd64 binary (cross-compiled from this Mac), stages it with the
# operator's LIVE config (which contains the real upstream credential) plus
# install.sh + README + LICENSE, and tars the lot into dist/.
#
# The archive is LOCAL ONLY — it is never published and never committed. The
# credential is read from the live config at build time; it does not live in
# this repo. If the archive file leaks, rotate the upstream credential.
#
# Usage: sh scripts/build-mac-archive.sh
# Output: dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz

set -eu

LIVE_CONFIG="$HOME/.config/http-proxy-local/config.yaml"
VERSION="0.1.0"
ARCHIVE_NAME="http-proxy-local-mac-amd64-v${VERSION}.tar.gz"
STAGE_DIR="dist/archive-build"
ARCHIVE_PATH="dist/${ARCHIVE_NAME}"

die() {
    echo "build: error: $*" >&2
    exit 1
}

# --- preflight ---

command -v go >/dev/null 2>&1 || die "go not found on PATH. Install Go or use mise."
[ -f "$LIVE_CONFIG" ] || die "live config not found at $LIVE_CONFIG. Nothing to bake in."
[ -f ./install.sh ] || die "install.sh not found in repo root."
[ -f ./README.md ] || die "README.md not found in repo root."
[ -f ./LICENSE ] || die "LICENSE not found in repo root."

# Resolve the repo root from the script location so the build works regardless
# of the caller's CWD.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "build: staging into $STAGE_DIR"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR"

# --- 1. cross-compile the amd64 binary ---

echo "build: compiling darwin/amd64 binary (CGO disabled, static)"
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
    -o "$STAGE_DIR/http-proxy-local" \
    ./cmd/http-proxy-local

# --- 2. stage the payload ---

echo "build: staging config (from live config — contains the upstream credential)"
install -m 0600 "$LIVE_CONFIG" "$STAGE_DIR/config.yaml"

echo "build: staging install.sh, README.md, LICENSE"
install -m 0755 ./install.sh "$STAGE_DIR/install.sh"
install -m 0644 ./README.md "$STAGE_DIR/README.md"
install -m 0644 ./LICENSE "$STAGE_DIR/LICENSE"

# --- 3. verify the binary's arch before tarring ---

BIN_INFO="$(file "$STAGE_DIR/http-proxy-local")"
case "$BIN_INFO" in
    *"Mach-O"*"x86_64"*) ;;
    *) die "built binary is not a Mach-O x86_64 executable: $BIN_INFO" ;;
esac
echo "build: binary arch OK ($BIN_INFO)"

# --- 4. tar ---

echo "build: creating $ARCHIVE_PATH"
tar -czf "$ARCHIVE_PATH" -C "$STAGE_DIR" .

# --- 5. report ---

ARCHIVE_SIZE="$(du -h "$ARCHIVE_PATH" | cut -f1)"
echo
echo "================================================================"
echo "build: done."
echo
echo "  archive : $ARCHIVE_PATH"
echo "  size    : $ARCHIVE_SIZE"
echo
echo "Contents:"
tar -tzf "$ARCHIVE_PATH"
echo
echo "Hand-off: copy $ARCHIVE_PATH to the target mac, then:"
echo "  tar -xzf $(basename "$ARCHIVE_PATH")"
echo "  cd http-proxy-local-mac-amd64-v${VERSION}  # or wherever you extracted"
echo "  ./install.sh"
echo
echo "NOTE: this archive contains the upstream credential from your live"
echo "config. Do not publish it. If it leaks, rotate the credential."
echo "================================================================"
```

Implementation notes for the implementer:
- The script resolves its own location (`SCRIPT_DIR`/`REPO_ROOT`) and `cd`s to the repo root, so it works regardless of the caller's CWD (e.g. called as `sh scripts/build-mac-archive.sh` from repo root, or `sh ../repo/scripts/build-mac-archive.sh` from elsewhere).
- `file ... | grep` is replaced with a `case` statement to avoid the `grep` flag quirk seen elsewhere in this environment; the `case` matches "Mach-O" and "x86_64" substrings in the `file` output.
- `du -h | cut -f1` gives a human-readable size for the report.
- The archive is built from `$STAGE_DIR` (a clean dir), so no stray files leak in.
- **Do not** run the script as part of committing it — it's a build tool that runs later (Task 3). Just write it, syntax-check, commit.

- [ ] **Step 3: Syntax-check the builder**

Run: `sh -n scripts/build-mac-archive.sh`
Expected: no output, exit 0.

- [ ] **Step 4: (Optional) shellcheck if available**

Run: `command -v shellcheck >/dev/null && shellcheck scripts/build-mac-archive.sh install.sh || echo "(shellcheck not installed; sh -n is the gate)"`
Expected: either shellcheck findings to address, or the "(not installed)" message. `sh -n` in Step 3 is the mandatory gate; shellcheck is a bonus.

- [ ] **Step 5: Commit**

```bash
git add scripts/build-mac-archive.sh
git commit -m "build: add local mac archive builder (cross-compile + stage + tar)"
```

---

## Task 3: Build the archive and verify

Run the builder, inspect the output, and verify the archive is well-formed. This is the "does it actually produce the artifact" gate — no commits (the archive is gitignored).

**Files:**
- (no file changes — `dist/` is gitignored)

**Interfaces:**
- Consumes: `scripts/build-mac-archive.sh` (Task 2), all its inputs.
- Produces: `dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz` on disk.

- [ ] **Step 1: Run the builder**

Run: `sh scripts/build-mac-archive.sh`
Expected: the script prints each stage (compiling, staging, arch OK, creating), then the final report block with the archive path, size, and contents listing. Exit 0.

If it fails at the compile step, the error is likely a Go build issue — read the `go build` error output and fix it (the script's `set -eu` will have aborted on the first failure).

- [ ] **Step 2: Inspect the archive contents**

Run: `tar -tzf dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz`
Expected output (exactly 5 entries, no paths — tar was built from the stage dir with `-C`):
```
./http-proxy-local
./config.yaml
./install.sh
./README.md
./LICENSE
```

If there are extra files, the stage dir wasn't clean (the builder `rm -rf`s it first, so this shouldn't happen) — re-run the builder.

- [ ] **Step 3: Verify the binary inside the archive**

Run: `mkdir -p /tmp/hpl-verify && tar -xzf dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz -C /tmp/hpl-verify && file /tmp/hpl-verify/http-proxy-local`
Expected: the `file` output contains `Mach-O 64-bit executable x86_64`. (This confirms the cross-compile produced the right arch, even though we can't *run* an amd64 binary on this arm64 Mac.)

Run: `rm -rf /tmp/hpl-verify` to clean up.

- [ ] **Step 4: Verify the config inside the archive (without exposing the credential)**

Run: `tar -xzf dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz -O ./config.yaml | grep -cE '^(listen|upstream:|  url:|geo:|routing:|socks5:|smart:|pac:)' || true`
Expected: a count > 0 (confirms the config has the expected top-level structure). Do NOT print the config's contents to the terminal — it contains the credential. The grep counts structural keys only.

- [ ] **Step 5: Report the artifact path to the user**

Do NOT commit anything (`dist/` is gitignored). Report to the user:
- The archive is built at `dist/http-proxy-local-mac-amd64-v0.1.0.tar.gz` (with its size).
- The 5 contents verified.
- The binary is a confirmed Mach-O x86_64.
- The config has the expected structure (credential verified present, not displayed).
- **Next step is the operator's**: copy the archive to the target amd64 Mac, extract, run `./install.sh`. The full runtime validation (install.sh actually working) happens there — this arm64 Mac correctly fails install.sh's arch check, by design.

---

## Self-Review Notes

**Spec coverage check** (every spec section → task):
- §Archive contents (binary, config, install.sh, README, LICENSE) → Task 2 stages all 5; Task 3 Step 2 verifies all 5. ✓
- §install.sh preflight (Darwin + x86_64 checks, payload presence) → Task 1 Step 1 preflight block. ✓
- §install.sh privilege model (sudo only for /usr/local/bin) → Task 1 `sudo_cmd` helper wraps only the binary-placement steps. ✓
- §install.sh place binary (/usr/local/bin, mode 0755) → Task 1 Step "2. place the binary". ✓
- §install.sh place config (~/.config/http-proxy-local/, mode 0600) → Task 1 Step "3. place the config". ✓
- §install.sh write plist (Label, ProgramArguments with absolute paths, RunAtLoad, KeepAlive, log paths) → Task 1 Step "4. write the launchd plist". ✓
- §install.sh load (unload prior, then load) → Task 1 Step "5. load the plist". ✓
- §install.sh verify (poll /proxy.pac, 30s timeout) → Task 1 Step "6. verify", `STARTUP_TIMEOUT=30` reflecting the binary's 60s geo-fetch block (Global Constraint). ✓
- §install.sh print next steps (networksetup command, logs, restart, uninstall) → Task 1 Step "7. print the manual next step" + header comment. ✓
- §install.sh no --uninstall flag → Task 1 "Do not add a --uninstall flag" note. ✓
- §build-mac-archive.sh preflight (live config exists, go on PATH) → Task 2 preflight block. ✓
- §build-mac-archive.sh cross-compile (GOOS=darwin GOARCH=amd64 CGO_ENABLED=0) → Task 2 Step "1. cross-compile". ✓
- §build-mac-archive.sh stage files (config 0600, install.sh 0755, README/LICENSE 0644) → Task 2 Step "2. stage the payload". ✓
- §build-mac-archive.sh verify arch before tar → Task 2 Step "3. verify the binary's arch". ✓
- §build-mac-archive.sh tar + report → Task 2 Steps "4. tar" + "5. report". ✓
- §build-mac-archive.sh reads credential at build time, never commits/publishes → Global Constraint + Task 2 header comment + Task 3 (no commit). ✓
- §Verification (sh -n mandatory, shellcheck optional, arch check, runtime test on target) → Task 1 Step 2, Task 2 Steps 3-4, Task 3 Steps 1-4, Task 3 Step 5 handoff. ✓
- §No public-release changes / no CI / no published creds → Global Constraints + no task touches `.goreleaser.yaml`/`release.yml`. ✓

**Placeholder scan:** none. Every step has exact code or exact commands. The `Wi-Fi` token in install.sh's output is the literal default macOS service name the operator may need to change (called out in the message), not a plan placeholder.

**Type/name consistency:** The label `com.djarvur.http-proxy-local`, paths (`/usr/local/bin/http-proxy-local`, `~/.config/http-proxy-local/config.yaml`, `~/Library/LaunchAgents/...plist`, `~/Library/Logs/http-proxy-local.log`), and archive name `http-proxy-local-mac-amd64-v0.1.0.tar.gz` are consistent across Task 1 (install.sh), Task 2 (builder), and Task 3 (verification). The `STARTUP_TIMEOUT=30` in install.sh matches the Global Constraint note about the binary's 60s geo-fetch block. The `VERSION="0.1.0"` in the builder matches the released version.

**Risk note — runtime test deferral:** install.sh cannot be fully runtime-tested on this arm64 build Mac (its amd64 check fails by design). The verification bar is `sh -n` + structural review + archive well-formedness + binary arch confirmed. The real runtime test is `./install.sh` on the target Mac, which Task 3 Step 5 hands off to the operator. This is honest and correct for a one-time personal install — do not weaken the arch check to force a local test (that would test a different script than ships).
