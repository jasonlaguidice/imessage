#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"
BUNDLE_ID="$3"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup (Beeper)"
echo "═══════════════════════════════════════════════"
echo ""

# ── Check bbctl ───────────────────────────────────────────────
BBCTL=""
for p in bbctl /usr/local/bin/bbctl "$HOME/.local/bin/bbctl"; do
    if command -v "$p" >/dev/null 2>&1 || [ -x "$p" ]; then
        BBCTL="$p"
        break
    fi
done
if [ -z "$BBCTL" ]; then
    echo "ERROR: bbctl not found."
    echo ""
    echo "  Install from: https://github.com/beeper/bridge-manager/releases"
    echo "  Then: chmod +x bbctl && sudo mv bbctl /usr/local/bin/"
    echo ""
    exit 1
fi
echo "✓ Found bbctl: $BBCTL"

# ── Check bbctl login ────────────────────────────────────────
if ! "$BBCTL" whoami >/dev/null 2>&1 || "$BBCTL" whoami 2>&1 | grep -q "not logged in"; then
    echo ""
    echo "Not logged into Beeper. Running bbctl login..."
    echo ""
    "$BBCTL" login
fi
WHOAMI=$("$BBCTL" whoami 2>&1 | head -1)
echo "✓ Logged in: $WHOAMI"

# ── Generate config via bbctl ─────────────────────────────────
mkdir -p "$DATA_DIR"
if [ -f "$CONFIG" ]; then
    echo "✓ Config already exists at $CONFIG"
    echo "  Delete it to regenerate from Beeper."
else
    echo "Generating Beeper config..."
    "$BBCTL" config --type bridgev2 sh-imessage > "$CONFIG"
    echo "✓ Config saved to $CONFIG"
fi

if ! grep -q "beeper" "$CONFIG" 2>/dev/null; then
    echo ""
    echo "WARNING: Config doesn't appear to contain Beeper details."
    echo "  Try: rm $CONFIG && re-run make install-beeper"
    echo ""
    exit 1
fi

# ── Install LaunchAgent ───────────────────────────────────────
CONFIG_ABS="$(cd "$DATA_DIR" && pwd)/config.yaml"
DATA_ABS="$(cd "$DATA_DIR" && pwd)"
LOG_OUT="$DATA_ABS/bridge.stdout.log"
LOG_ERR="$DATA_ABS/bridge.stderr.log"

mkdir -p "$(dirname "$PLIST")"
launchctl unload "$PLIST" 2>/dev/null || true

cat > "$PLIST" << PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$BUNDLE_ID</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BINARY</string>
        <string>-c</string>
        <string>$CONFIG_ABS</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DATA_ABS</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$LOG_OUT</string>
    <key>StandardErrorPath</key>
    <string>$LOG_ERR</string>
</dict>
</plist>
PLIST_EOF

launchctl load "$PLIST"
echo "✓ Bridge started (LaunchAgent installed)"
echo ""

# ── Wait for bridge to connect ────────────────────────────────
DOMAIN=$(grep '^\s*domain:' "$CONFIG" | head -1 | awk '{print $2}')
DOMAIN="${DOMAIN:-beeper.local}"

echo "Waiting for bridge to start..."
for i in $(seq 1 15); do
    if grep -q "Bridge started\|UNCONFIGURED\|Backfill queue starting" "$LOG_OUT" 2>/dev/null; then
        echo "✓ Bridge is running"
        echo ""
        echo "═══════════════════════════════════════════════"
        echo "  Next: Open Beeper and DM"
        echo "    @sh-imessagebot:$DOMAIN"
        echo "  Send: login"
        echo "═══════════════════════════════════════════════"
        echo ""
        echo "Logs: tail -f $LOG_OUT"
        exit 0
    fi
    sleep 1
done

echo ""
echo "Bridge is starting up (check logs for status):"
echo "  tail -f $LOG_OUT"
echo ""
echo "Once running, DM @sh-imessagebot:$DOMAIN and send: login"
