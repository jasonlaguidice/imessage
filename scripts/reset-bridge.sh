#!/usr/bin/env bash
#
# Full bridge reset: delete Beeper registration, wipe ALL local state.
# You will need to re-login (2FA) after reset.
#
# Usage: make reset   (run interactively — prompts for confirmation)
#
set -euo pipefail

STATE_DIR="$HOME/.local/share/mautrix-imessage"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$STATE_DIR/bridge-manager/bbctl"

# ── Preflight checks ────────────────────────────────────────
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

sleep 1
if pgrep -f mautrix-imessage-v2 >/dev/null 2>&1; then
    echo "ERROR: bridge process still running after stop"
    exit 1
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
echo ""
echo "Deleting bridge registration from Beeper..."
echo "(Answer the confirmation prompt below)"
echo ""
"$BBCTL" delete "$BRIDGE_NAME"

# ── Wipe EVERYTHING ─────────────────────────────────────────
echo ""
echo "Wiping all state in $STATE_DIR/ ..."
find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" -exec rm -rf {} +

# Verify
REMAINING=$(find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" | wc -l)
if [ "$REMAINING" -ne 0 ]; then
    echo "ERROR: state directory not fully cleaned:"
    ls -la "$STATE_DIR/"
    exit 1
fi

echo ""
echo "✓ Bridge fully reset."
echo "  All state wiped — you will need to re-login (2FA)."
echo ""
echo "  Run 'make install-beeper' to re-register, login, and start the bridge."
