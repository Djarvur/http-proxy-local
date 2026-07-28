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
