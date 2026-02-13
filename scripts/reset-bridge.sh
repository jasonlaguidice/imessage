#!/usr/bin/env bash
#
# Reset the bridge: wipe all Matrix rooms and synced data, re-register with
# Beeper, and re-sync from CloudKit. Keeps the iCloud login intact.
#
# Usage: make reset
#
set -euo pipefail

DATA_DIR="${1:-$(pwd)/data}"
BUNDLE_ID="${2:-com.lrhodin.mautrix-imessage}"
CONFIG="$DATA_DIR/config.yaml"
BRIDGE_NAME="sh-imessage"
BBCTL_LINUX="$HOME/.local/share/mautrix-imessage/bridge-manager/bbctl"
UNAME_S=$(uname -s)

if [ "$UNAME_S" = "Darwin" ]; then
    BBCTL="$HOME/.local/share/mautrix-imessage/bridge-manager/bbctl"
else
    BBCTL="$BBCTL_LINUX"
fi

if [ ! -x "$BBCTL" ]; then
    echo "ERROR: bbctl not found at $BBCTL"
    exit 1
fi

DB_URI=""
if [ -f "$CONFIG" ]; then
    DB_URI=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
fi

if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    echo "ERROR: No existing bridge database found."
    echo "  Run 'make install-beeper' first."
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

# ── Re-register and get fresh config ─────────────────────────
echo "Re-registering bridge with Beeper..."
rm -f "$CONFIG"
"$BBCTL" config --type imessage-v2 -o "$CONFIG" "$BRIDGE_NAME"

# Patch config: absolute DB path
if [ "$UNAME_S" = "Darwin" ]; then
    DATA_ABS="$(cd "$DATA_DIR" && pwd)"
    sed -i '' "s|uri: file:mautrix-imessage.db|uri: file:$DATA_ABS/mautrix-imessage.db|" "$CONFIG"
    sed -i '' 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
else
    sed -i "s|uri: file:mautrix-imessage.db|uri: file:$DATA_DIR/mautrix-imessage.db|" "$CONFIG"
    sed -i 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
fi

# ── Wipe synced data but keep user_login ─────────────────────
echo "Clearing synced data (keeping iCloud login)..."
for table in portal ghost user_portal message reaction disappearing_message \
             backfill_task cloud_chat cloud_message cloud_sync_state cloud_repair_task; do
    sqlite3 "$DB_URI" "DELETE FROM $table;" 2>/dev/null || true
done
echo "✓ Cleared portals, messages, cloud sync state, and backfill tasks"

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
