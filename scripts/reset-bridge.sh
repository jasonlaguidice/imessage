#!/usr/bin/env bash
#
# Reset the bridge: wipe all Matrix rooms and synced data, re-register with
# Beeper, and re-sync from CloudKit. Keeps the iCloud login intact (session
# files in ~/.local/share/mautrix-imessage/ are untouched).
#
# Usage: make reset
#
set -euo pipefail

DATA_DIR="${1:-$(pwd)/data}"
BUNDLE_ID="${2:-com.lrhodin.mautrix-imessage}"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)

if [ "$UNAME_S" = "Darwin" ]; then
    BBCTL="$HOME/.local/share/mautrix-imessage/bridge-manager/bbctl"
else
    BBCTL="$HOME/.local/share/mautrix-imessage/bridge-manager/bbctl"
fi

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
echo "  (confirm the deletion when prompted)"
"$BBCTL" delete "$BRIDGE_NAME" || echo "  (bridge may already be unregistered)"

# ── Wipe the bridge DB entirely ──────────────────────────────
# The iCloud login is persisted separately in session.json + keystore.plist
# and will be auto-restored on next start.
echo "Removing bridge database..."
rm -f "$DATA_DIR"/mautrix-imessage.db*

# ── Re-register and get fresh config ─────────────────────────
echo "Re-registering bridge with Beeper..."
rm -f "$DATA_DIR/config.yaml"
for i in 1 2 3 4 5 6; do
    if "$BBCTL" config --type imessage-v2 -o "$DATA_DIR/config.yaml" "$BRIDGE_NAME" 2>/dev/null; then
        break
    fi
    echo "  Waiting for deletion to complete... (attempt $i/6)"
    sleep 5
done
if [ ! -f "$DATA_DIR/config.yaml" ]; then
    echo "ERROR: Failed to re-register bridge after 30s. Try again in a minute."
    exit 1
fi

# Patch config
if [ "$UNAME_S" = "Darwin" ]; then
    DATA_ABS="$(cd "$DATA_DIR" && pwd)"
    sed -i '' "s|uri: file:mautrix-imessage.db|uri: file:$DATA_ABS/mautrix-imessage.db|" "$DATA_DIR/config.yaml"
    sed -i '' 's/max_batches: 0$/max_batches: -1/' "$DATA_DIR/config.yaml"
else
    sed -i "s|uri: file:mautrix-imessage.db|uri: file:$DATA_DIR/mautrix-imessage.db|" "$DATA_DIR/config.yaml"
    sed -i 's/max_batches: 0$/max_batches: -1/' "$DATA_DIR/config.yaml"
fi

# ── Restart ──────────────────────────────────────────────────
echo "Starting bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    launchctl load "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user start mautrix-imessage 2>/dev/null || true
fi

echo ""
echo "✓ Bridge reset complete. It will re-sync from CloudKit now."
echo "  Monitor with: journalctl --user -u mautrix-imessage -f"
