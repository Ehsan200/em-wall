#!/usr/bin/env bash
# em-wall installer. Idempotent — safe to re-run.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "this script must be run as root (sudo)" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLIST_SRC="$REPO_ROOT/launchd/com.em-wall.daemon.plist"
PLIST_DST="/Library/LaunchDaemons/com.em-wall.daemon.plist"
ANCHOR_FILE="/etc/pf.anchors/em-wall"
PF_CONF="/etc/pf.conf"
DAEMON_BIN_DST="/usr/local/bin/em-walld"
LOG_DIR="/usr/local/var/log"
DB_DIR="/usr/local/var/em-wall"

echo "==> building em-walld"
pushd "$REPO_ROOT" >/dev/null
go build -o "$REPO_ROOT/build/em-walld" ./daemon
popd >/dev/null

echo "==> installing binary to $DAEMON_BIN_DST"
install -m 0755 "$REPO_ROOT/build/em-walld" "$DAEMON_BIN_DST"

echo "==> creating data and log dirs"
mkdir -p "$DB_DIR" "$LOG_DIR"
chmod 0755 "$DB_DIR" "$LOG_DIR"

echo "==> installing pf anchor stub at $ANCHOR_FILE"
mkdir -p "$(dirname "$ANCHOR_FILE")"
# Empty anchor file — daemon rewrites it via pfctl -f -.
[ -e "$ANCHOR_FILE" ] || : > "$ANCHOR_FILE"
chmod 0644 "$ANCHOR_FILE"

if ! grep -q '^anchor "em-wall"' "$PF_CONF"; then
    echo "==> patching $PF_CONF (adding em-wall anchor)"
    cp "$PF_CONF" "$PF_CONF.em-wall.bak.$(date +%s)"
    {
        echo ''
        echo '# em-wall: encrypted DNS blocking anchor'
        echo 'anchor "em-wall"'
        echo 'load anchor "em-wall" from "/etc/pf.anchors/em-wall"'
    } >> "$PF_CONF"
    pfctl -f "$PF_CONF" || echo "warning: pfctl reload failed (pf may not be enabled yet)"
else
    echo "==> $PF_CONF already references em-wall anchor"
fi

echo "==> ensuring pf is enabled"
pfctl -e 2>/dev/null || true

echo "==> installing LaunchDaemon plist"
install -m 0644 -o root -g wheel "$PLIST_SRC" "$PLIST_DST"

echo "==> loading daemon"
launchctl bootout system "$PLIST_DST" 2>/dev/null || true
launchctl bootstrap system "$PLIST_DST"
launchctl enable system/com.em-wall.daemon
launchctl kickstart -k system/com.em-wall.daemon

sleep 1
if launchctl print system/com.em-wall.daemon 2>/dev/null | grep -q 'state = running'; then
    echo "==> em-walld is running"
else
    echo "WARNING: daemon did not enter running state. Check $LOG_DIR/em-wall.log"
fi

echo
echo "next steps:"
echo "  1. point system DNS at 127.0.0.1:"
echo "       sudo networksetup -setdnsservers Wi-Fi 127.0.0.1"
echo "     (replace 'Wi-Fi' with your active service name; see networksetup -listallnetworkservices)"
echo "  2. launch the UI:"
echo "       cd $REPO_ROOT/app && ~/go/bin/wails dev"
echo
