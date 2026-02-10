#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"
BUNDLE_ID="$3"

BRIDGE_NAME="${BRIDGE_NAME:-sh-imessage}"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"

# Where we build/cache bbctl
BBCTL_DIR="${BBCTL_DIR:-$HOME/.local/share/mautrix-imessage/bridge-manager}"
BBCTL_REPO="${BBCTL_REPO:-https://github.com/lrhodin/bridge-manager.git}"
BBCTL_BRANCH="${BBCTL_BRANCH:-add-imessage-v2}"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup (Beeper)"
echo "═══════════════════════════════════════════════"
echo ""

# ── Build bbctl from source ───────────────────────────────────
BBCTL="$BBCTL_DIR/bbctl"

build_bbctl() {
    echo "Building bbctl..."
    mkdir -p "$(dirname "$BBCTL_DIR")"
    if [ -d "$BBCTL_DIR" ]; then
        cd "$BBCTL_DIR"
        git fetch --quiet origin
        git checkout --quiet "$BBCTL_BRANCH"
        git reset --hard --quiet "origin/$BBCTL_BRANCH"
    else
        git clone --quiet --branch "$BBCTL_BRANCH" "$BBCTL_REPO" "$BBCTL_DIR"
        cd "$BBCTL_DIR"
    fi
    go build -o bbctl ./cmd/bbctl/ 2>&1
    cd - >/dev/null
    echo "✓ Built bbctl"
}

if [ ! -x "$BBCTL" ]; then
    build_bbctl
else
    echo "✓ Found bbctl: $BBCTL"
    # Update if repo has changes
    if [ -d "$BBCTL_DIR/.git" ]; then
        cd "$BBCTL_DIR"
        git fetch --quiet origin 2>/dev/null || true
        LOCAL=$(git rev-parse HEAD 2>/dev/null)
        REMOTE=$(git rev-parse "origin/$BBCTL_BRANCH" 2>/dev/null || echo "$LOCAL")
        cd - >/dev/null
        if [ "$LOCAL" != "$REMOTE" ]; then
            echo "  Updating bbctl..."
            build_bbctl
        fi
    fi
fi

# ── Check bbctl login ────────────────────────────────────────
if ! "$BBCTL" whoami >/dev/null 2>&1 || "$BBCTL" whoami 2>&1 | grep -qi "not logged in"; then
    echo ""
    echo "Not logged into Beeper. Running bbctl login..."
    echo ""
    "$BBCTL" login
fi
WHOAMI=$("$BBCTL" whoami 2>&1 | head -1 || true)
echo "✓ Logged in: $WHOAMI"

# ── Generate config via bbctl ─────────────────────────────────
mkdir -p "$DATA_DIR"
if [ -f "$CONFIG" ]; then
    echo "✓ Config already exists at $CONFIG"
    echo "  Delete it to regenerate from Beeper."
else
    echo "Generating Beeper config..."
    "$BBCTL" config --type imessage-v2 -o "$CONFIG" "$BRIDGE_NAME"
    # Make DB path absolute so it doesn't depend on working directory
    DATA_ABS_TMP="$(cd "$DATA_DIR" && pwd)"
    sed -i '' "s|uri: file:mautrix-imessage.db|uri: file:$DATA_ABS_TMP/mautrix-imessage.db|" "$CONFIG"
    echo "✓ Config saved to $CONFIG"
fi

if ! grep -q "beeper" "$CONFIG" 2>/dev/null; then
    echo ""
    echo "WARNING: Config doesn't appear to contain Beeper details."
    echo "  Try: rm $CONFIG && re-run make install-beeper"
    echo ""
    exit 1
fi

# ── Check for existing login / prompt if needed ──────────────
DB_URI=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
NEEDS_LOGIN=false

if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    NEEDS_LOGIN=true
elif command -v sqlite3 >/dev/null 2>&1; then
    LOGIN_COUNT=$(sqlite3 "$DB_URI" "SELECT count(*) FROM user_login;" 2>/dev/null || echo "0")
    if [ "$LOGIN_COUNT" = "0" ]; then
        NEEDS_LOGIN=true
    fi
fi

if [ "$NEEDS_LOGIN" = "true" ]; then
    echo ""
    echo "  ℹ No iMessage login found."
    echo "  Start the bridge and log in through Beeper:"
    echo "    1. Start the bridge (launchd will do this automatically)"
    echo "    2. Open Beeper and find the iMessage bridge bot"
    echo "    3. Send 'login' to start the login flow"
    echo ""
fi

# ── Full Disk Access check ────────────────────────────────────
APP_BUNDLE="$(dirname "$(dirname "$(dirname "$BINARY")")")"
if ! sqlite3 "$HOME/Library/Messages/chat.db" "SELECT COUNT(*) FROM message LIMIT 1;" >/dev/null 2>&1; then
    echo ""
    echo "┌─────────────────────────────────────────────┐"
    echo "│  Full Disk Access Required                  │"
    echo "│                                             │"
    echo "│  1. System Settings → Privacy & Security    │"
    echo "│     → Full Disk Access                      │"
    echo "│  2. Click + and add:                        │"
    echo "│     $APP_BUNDLE"
    echo "│  3. Also add Terminal.app if not present    │"
    echo "│                                             │"
    echo "│  Opening System Settings now...             │"
    echo "└─────────────────────────────────────────────┘"
    echo ""
    open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"
    read -p "Press Enter once Full Disk Access is granted..."
fi

# ── Install LaunchAgent ───────────────────────────────────────
CONFIG_ABS="$(cd "$DATA_DIR" && pwd)/config.yaml"
DATA_ABS="$(cd "$DATA_DIR" && pwd)"
LOG_OUT="$DATA_ABS/bridge.stdout.log"
LOG_ERR="$DATA_ABS/bridge.stderr.log"

mkdir -p "$(dirname "$PLIST")"
GUI_DOMAIN="gui/$(id -u)"
launchctl bootout "$GUI_DOMAIN/$BUNDLE_ID" 2>/dev/null || true
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
    <dict>
        <key>Crashed</key>
        <true/>
    </dict>
    <key>StandardOutPath</key>
    <string>$LOG_OUT</string>
    <key>StandardErrorPath</key>
    <string>$LOG_ERR</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin</string>
        <key>CGO_CFLAGS</key>
        <string>-I/opt/homebrew/include</string>
        <key>CGO_LDFLAGS</key>
        <string>-L/opt/homebrew/lib</string>
    </dict>
</dict>
</plist>
PLIST_EOF

if ! launchctl bootstrap "$GUI_DOMAIN" "$PLIST" 2>/dev/null; then
    if ! launchctl load "$PLIST" 2>/dev/null; then
        echo ""
        echo "⚠  LaunchAgent failed to load. You can run the bridge manually:"
        echo "   $BINARY -c $CONFIG_ABS"
        echo ""
        echo "   This is a known issue on macOS 13 (Ventura). Try:"
        echo "   1. Remove and re-add the .app in Full Disk Access"
        echo "   2. Re-run: make install-beeper"
        echo ""
    fi
fi
echo "✓ Bridge started (LaunchAgent installed)"
echo ""

# ── Wait for bridge to connect ────────────────────────────────
DOMAIN=$(grep '^\s*domain:' "$CONFIG" | head -1 | awk '{print $2}' || true)
DOMAIN="${DOMAIN:-beeper.local}"

echo "Waiting for bridge to start..."
for i in $(seq 1 15); do
    if grep -q "Bridge started\|UNCONFIGURED\|Backfill queue starting" "$LOG_OUT" 2>/dev/null; then
        echo "✓ Bridge is running"
        echo ""
        echo "═══════════════════════════════════════════════"
        echo "  Setup Complete"
        echo "═══════════════════════════════════════════════"
        echo ""
        echo "  Logs:    tail -f $LOG_OUT"
        echo "  Stop:    launchctl bootout $GUI_DOMAIN/$BUNDLE_ID"
        echo "  Start:   launchctl bootstrap $GUI_DOMAIN $PLIST"
        echo "  Restart: launchctl kickstart -k $GUI_DOMAIN/$BUNDLE_ID"
        exit 0
    fi
    sleep 1
done

echo ""
echo "Bridge is starting up (check logs for status):"
echo "  tail -f $LOG_OUT"
echo ""
echo "Once running, DM @${BRIDGE_NAME}bot:$DOMAIN and send: login"
