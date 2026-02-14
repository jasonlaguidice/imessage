#!/usr/bin/env bash
#
# Reset the bridge: delete Beeper registration, wipe DB and config.
# Run "make install-beeper" after to re-register and start fresh.
#
# iCloud login is preserved (session.json, keystore.plist, trustedpeers.plist).
#
# Usage: make reset
#
set -euo pipefail

STATE_DIR="$HOME/.local/share/mautrix-imessage"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$STATE_DIR/bridge-manager/bbctl"

if [ ! -x "$BBCTL" ]; then
    echo "ERROR: bbctl not found at $BBCTL"
    exit 1
fi

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    BUNDLE_ID="${1:-com.lrhodin.mautrix-imessage}"
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop mautrix-imessage 2>/dev/null || true
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
echo "Deleting bridge registration from Beeper..."
if command -v tmux >/dev/null 2>&1; then
    tmux kill-session -t _bbctl_del 2>/dev/null || true
    tmux new-session -d -s _bbctl_del "$BBCTL delete $BRIDGE_NAME; sleep 2"
    sleep 2
    tmux send-keys -t _bbctl_del 'y' Enter
    for i in $(seq 1 15); do
        if ! tmux has-session -t _bbctl_del 2>/dev/null; then break; fi
        sleep 1
    done
    tmux kill-session -t _bbctl_del 2>/dev/null || true
else
    "$BBCTL" delete "$BRIDGE_NAME" || echo "  (bridge may already be unregistered)"
fi

# ── Wipe DB and config, preserve iCloud login ────────────────
echo "Removing bridge database and config..."
rm -f "$STATE_DIR"/mautrix-imessage.db*
rm -f "$STATE_DIR"/cloudkit_chats_dump.json
rm -f "$STATE_DIR/config.yaml"

echo ""
echo "✓ Bridge reset complete."
echo "  iCloud login preserved in $STATE_DIR/"
echo ""
echo "  Run 'make install-beeper' to re-register and start the bridge."
