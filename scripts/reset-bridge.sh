#!/usr/bin/env bash
#
# Full bridge reset: delete Beeper registration, wipe ALL local state.
# You will need to re-login (2FA) after reset.
#
# Usage: make reset
#
set -euo pipefail

STATE_DIR="$HOME/.local/share/mautrix-imessage"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$STATE_DIR/bridge-manager/bbctl"

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    BUNDLE_ID="${1:-com.lrhodin.mautrix-imessage}"
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop mautrix-imessage 2>/dev/null || true
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
if [ -x "$BBCTL" ]; then
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
else
    echo "ERROR: bbctl not found at $BBCTL"
    echo "  Server-side rooms must be cleaned up before wiping local state."
    exit 1
fi

# ── Wipe EVERYTHING ─────────────────────────────────────────
echo "Wiping all state in $STATE_DIR/ ..."
# Keep bridge-manager (bbctl binary) — it's just a build cache
find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" -exec rm -rf {} +

echo ""
echo "✓ Bridge fully reset."
echo "  All state wiped — you will need to re-login (2FA)."
echo ""
echo "  Run 'make install-beeper' to re-register, login, and start the bridge."
