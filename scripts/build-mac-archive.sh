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
