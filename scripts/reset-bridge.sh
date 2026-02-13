#!/usr/bin/env bash
#
# Reset the bridge: delete Beeper registration, wipe DB and config.
# Run "make install-beeper" after to re-register and start fresh.
#
# iCloud login is preserved (session files in ~/.local/share/mautrix-imessage/).
#
# Usage: make reset
#
set -euo pipefail

DATA_DIR="${1:-$(pwd)/data}"
BUNDLE_ID="${2:-com.lrhodin.mautrix-imessage}"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$HOME/.local/share/mautrix-imessage/bridge-manager/bbctl"

if [ ! -x "$BBCTL" ]; then
    echo "ERROR: bbctl not found at $BBCTL"
    exit 1
fi

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop mautrix-imessage 2>/dev/null || true
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
# bbctl delete uses a survey prompt requiring a real TTY.
# We use 'expect'-style approach: run in a tmux session and send "y".
echo "Deleting bridge registration from Beeper..."
if command -v tmux >/dev/null 2>&1; then
    tmux kill-session -t _bbctl_del 2>/dev/null || true
    tmux new-session -d -s _bbctl_del "$BBCTL delete $BRIDGE_NAME; sleep 2"
    sleep 2
    # Send "y" + Enter to the confirmation prompt
    tmux send-keys -t _bbctl_del 'y' Enter
    # Wait for it to finish
    for i in $(seq 1 15); do
        if ! tmux has-session -t _bbctl_del 2>/dev/null; then break; fi
        sleep 1
    done
    tmux kill-session -t _bbctl_del 2>/dev/null || true
else
    "$BBCTL" delete "$BRIDGE_NAME" || echo "  (bridge may already be unregistered)"
fi

# ── Wipe local state ─────────────────────────────────────────
echo "Removing bridge database and config..."
rm -f "$DATA_DIR"/mautrix-imessage.db*
rm -f "$DATA_DIR/config.yaml"

echo ""
echo "✓ Bridge reset complete."
echo "  iCloud login preserved in ~/.local/share/mautrix-imessage/"
echo ""
echo "  Run 'make install-beeper' to re-register and start the bridge."
