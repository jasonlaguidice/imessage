#!/usr/bin/env bash
#
# Reset the bridge's synced data while keeping the login intact.
# This wipes cloud sync state, portals, and backfill tasks so everything
# re-syncs from CloudKit on next start. No re-login needed.
#
# Usage: make reset
#
set -euo pipefail

DATA_DIR="${1:-$(pwd)/data}"
CONFIG="$DATA_DIR/config.yaml"

if [ ! -f "$CONFIG" ]; then
    echo "ERROR: No config found at $CONFIG"
    echo "  Run 'make install-beeper' first."
    exit 1
fi

DB_URI=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    echo "ERROR: Database not found at $DB_URI"
    echo "  Run 'make install-beeper' first."
    exit 1
fi

# Stop the bridge
UNAME_S=$(uname -s)
if [ "$UNAME_S" = "Darwin" ]; then
    BUNDLE_ID="${2:-com.lrhodin.mautrix-imessage}"
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop mautrix-imessage 2>/dev/null || true
fi

echo "Resetting bridge sync data (keeping login)..."

# Wipe synced data but keep user_login and config
# Use DELETE FROM for each table individually — some may not exist yet
for table in portal ghost user_portal message reaction disappearing_message \
             backfill_task cloud_chat cloud_message cloud_sync_state cloud_repair_task; do
    sqlite3 "$DB_URI" "DELETE FROM $table;" 2>/dev/null || true
done

echo "✓ Cleared portals, messages, cloud sync state, and backfill tasks"
echo "  Login preserved — bridge will re-sync from CloudKit on next start."

# Restart
if [ "$UNAME_S" = "Darwin" ]; then
    launchctl load "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user start mautrix-imessage 2>/dev/null || true
fi

echo "✓ Bridge restarted"
