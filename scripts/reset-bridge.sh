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
echo "Deleting bridge registration from Beeper..."
"$BBCTL" delete "$BRIDGE_NAME" || echo "  (bridge may already be unregistered)"

# ── Wipe local state ─────────────────────────────────────────
echo "Removing bridge database and config..."
rm -f "$DATA_DIR"/mautrix-imessage.db*
rm -f "$DATA_DIR/config.yaml"

echo ""
echo "✓ Bridge reset complete."
echo "  iCloud login preserved in ~/.local/share/mautrix-imessage/"
echo ""
echo "  Run 'make install-beeper' to re-register and start the bridge."
