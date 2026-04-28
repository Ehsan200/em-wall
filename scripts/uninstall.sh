#!/usr/bin/env bash
# em-wall uninstaller. Removes the daemon and pf anchor reference.
# Leaves the SQLite DB and logs alone unless --purge is given.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "this script must be run as root (sudo)" >&2
    exit 1
fi

PURGE=0
[[ "${1:-}" == "--purge" ]] && PURGE=1

PLIST_DST="/Library/LaunchDaemons/com.em-wall.daemon.plist"
DAEMON_BIN_DST="/usr/local/bin/em-walld"
ANCHOR_FILE="/etc/pf.anchors/em-wall"
PF_CONF="/etc/pf.conf"
SOCKET="/var/run/em-wall.sock"
DB_DIR="/usr/local/var/em-wall"
LOG_FILE="/usr/local/var/log/em-wall.log"

echo "==> stopping daemon"
launchctl bootout system "$PLIST_DST" 2>/dev/null || true

echo "==> flushing pf anchor"
pfctl -a em-wall -F all 2>/dev/null || true

echo "==> removing files"
rm -f "$PLIST_DST" "$DAEMON_BIN_DST" "$SOCKET" "$ANCHOR_FILE"

if grep -qE '^(rdr-)?anchor "em-wall"' "$PF_CONF"; then
    echo "==> stripping em-wall lines from $PF_CONF"
    cp "$PF_CONF" "$PF_CONF.em-wall.uninstall.$(date +%s)"
    sed -i.tmp '/em-wall: anchors for DNS hijack/d;/em-wall: encrypted DNS blocking anchor/d;/^rdr-anchor "em-wall"$/d;/^anchor "em-wall"$/d;/^load anchor "em-wall" from /d' "$PF_CONF"
    rm -f "$PF_CONF.tmp"
    pfctl -f "$PF_CONF" || true
fi

if (( PURGE )); then
    echo "==> --purge: removing DB and logs"
    rm -rf "$DB_DIR" "$LOG_FILE"
fi

echo "==> done"
echo "remember to restore your system DNS:"
echo "  sudo networksetup -setdnsservers Wi-Fi empty"
