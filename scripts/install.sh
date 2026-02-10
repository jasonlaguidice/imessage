#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"
BUNDLE_ID="$3"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
REGISTRATION="$DATA_DIR/registration.yaml"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup"
echo "═══════════════════════════════════════════════"
echo ""

# ── Prompt for config values ──────────────────────────────────
FIRST_RUN=false
if [ -f "$CONFIG" ]; then
    echo "Config already exists at $CONFIG"
    echo "Skipping configuration prompts. Delete it to re-configure."
    echo ""
else
    FIRST_RUN=true

    read -p "Homeserver URL [http://localhost:8008]: " HS_ADDRESS
    HS_ADDRESS="${HS_ADDRESS:-http://localhost:8008}"

    read -p "Homeserver domain (the server_name, e.g. example.com): " HS_DOMAIN
    if [ -z "$HS_DOMAIN" ]; then
        echo "ERROR: Domain is required." >&2
        exit 1
    fi

    read -p "Your Matrix ID [@you:$HS_DOMAIN]: " ADMIN_USER
    ADMIN_USER="${ADMIN_USER:-@you:$HS_DOMAIN}"

    echo ""
    echo "Database:"
    echo "  1) PostgreSQL (recommended)"
    echo "  2) SQLite"
    read -p "Choice [1]: " DB_CHOICE
    DB_CHOICE="${DB_CHOICE:-1}"

    if [ "$DB_CHOICE" = "1" ]; then
        DB_TYPE="postgres"
        read -p "PostgreSQL URI [postgres://localhost/mautrix_imessage?sslmode=disable]: " DB_URI
        DB_URI="${DB_URI:-postgres://localhost/mautrix_imessage?sslmode=disable}"
    else
        DB_TYPE="sqlite3-fk-wal"
        DB_URI="file:$DATA_DIR/mautrix-imessage.db?_txlock=immediate"
    fi

    echo ""

    # ── Generate config ───────────────────────────────────────────
    mkdir -p "$DATA_DIR"
    "$BINARY" -c "$CONFIG" -e 2>/dev/null
    echo "✓ Generated config"

    # Patch values into the generated config
    python3 -c "
import re, sys
text = open('$CONFIG').read()

def patch(text, key, val):
    return re.sub(
        r'^(\s+' + re.escape(key) + r'\s*:)\s*.*$',
        r'\1 ' + val,
        text, count=1, flags=re.MULTILINE
    )

text = patch(text, 'address', '$HS_ADDRESS')
text = patch(text, 'domain', '$HS_DOMAIN')
text = patch(text, 'type', '$DB_TYPE')
text = patch(text, 'uri', '$DB_URI')

lines = text.split('\n')
in_perms = False
for i, line in enumerate(lines):
    if 'permissions:' in line and not line.strip().startswith('#'):
        in_perms = True
        continue
    if in_perms and line.strip() and not line.strip().startswith('#'):
        indent = len(line) - len(line.lstrip())
        lines[i] = ' ' * indent + '\"$ADMIN_USER\": admin'
        break
text = '\n'.join(lines)

open('$CONFIG', 'w').write(text)
"
    echo "✓ Configured: $HS_ADDRESS, $HS_DOMAIN, $ADMIN_USER, $DB_TYPE"
fi

# ── Read domain from config (works on first run and re-runs) ──
HS_DOMAIN=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+domain:\s*(\S+)', text, re.MULTILINE)
print(m.group(1) if m else 'yourserver')
")

# ── Generate registration ────────────────────────────────────
if [ -f "$REGISTRATION" ]; then
    echo "✓ Registration already exists"
else
    "$BINARY" -c "$CONFIG" -g -r "$REGISTRATION" 2>/dev/null
    echo "✓ Generated registration"
fi

# ── Register with homeserver (first run only) ─────────────────
if [ "$FIRST_RUN" = true ]; then
    REG_PATH="$(cd "$DATA_DIR" && pwd)/registration.yaml"
    echo ""
    echo "┌─────────────────────────────────────────────┐"
    echo "│  Register with your homeserver:             │"
    echo "│                                             │"
    echo "│  Add to homeserver.yaml:                    │"
    echo "│    app_service_config_files:                │"
    echo "│      - $REG_PATH"
    echo "│                                             │"
    echo "│  Then restart your homeserver.              │"
    echo "└─────────────────────────────────────────────┘"
    echo ""
    read -p "Press Enter once your homeserver is restarted..."
fi

# ── Backfill window (first install only) ──────────────────────
DB_PATH=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+uri:\s*file:([^?]+)', text, re.MULTILINE)
print(m.group(1) if m else '')
")
if [ -t 0 ] && { [ -z "$DB_PATH" ] || [ ! -f "$DB_PATH" ]; }; then
    CURRENT_DAYS=$(grep 'initial_sync_days:' "$CONFIG" | head -1 | sed 's/.*initial_sync_days: *//')
    [ -z "$CURRENT_DAYS" ] && CURRENT_DAYS=365
    printf "How many days of message history to backfill? [%s]: " "$CURRENT_DAYS"
    read BACKFILL_DAYS
    BACKFILL_DAYS=$(echo "$BACKFILL_DAYS" | tr -dc '0-9')
    [ -z "$BACKFILL_DAYS" ] && BACKFILL_DAYS="$CURRENT_DAYS"
    sed -i '' "s/initial_sync_days: .*/initial_sync_days: $BACKFILL_DAYS/" "$CONFIG"
    echo "✓ Backfill window set to $BACKFILL_DAYS days"
fi

# ── Check for existing login / prompt if needed ──────────────
DB_URI=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+uri:\s*file:([^?]+)', text, re.MULTILINE)
print(m.group(1) if m else '')
")
NEEDS_LOGIN=false

if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    NEEDS_LOGIN=true
elif command -v sqlite3 >/dev/null 2>&1; then
    LOGIN_COUNT=$(sqlite3 "$DB_URI" "SELECT count(*) FROM user_login;" 2>/dev/null || echo "0")
    if [ "$LOGIN_COUNT" = "0" ]; then
        NEEDS_LOGIN=true
    fi
else
    # sqlite3 not available — can't verify DB has logins, assume login needed
    NEEDS_LOGIN=true
fi

# ── Restore preferred_handle from DB or session backup ────────
if [ "$NEEDS_LOGIN" = "false" ]; then
    CURRENT_HANDLE=$(grep 'preferred_handle:' "$CONFIG" 2>/dev/null | head -1 | sed "s/.*preferred_handle: *//;s/['\"]//g" || true)
    if [ -z "$CURRENT_HANDLE" ]; then
        # Try DB first
        SAVED_HANDLE=""
        if command -v sqlite3 >/dev/null 2>&1 && [ -n "$DB_URI" ] && [ -f "$DB_URI" ]; then
            SAVED_HANDLE=$(sqlite3 "$DB_URI" "SELECT json_extract(metadata, '$.preferred_handle') FROM user_login LIMIT 1;" 2>/dev/null || true)
        fi
        # Fall back to session backup file
        if [ -z "$SAVED_HANDLE" ]; then
            SESSION_FILE="${XDG_DATA_HOME:-$HOME/.local/share}/mautrix-imessage/session.json"
            if [ -f "$SESSION_FILE" ] && command -v python3 >/dev/null 2>&1; then
                SAVED_HANDLE=$(python3 -c "import json; print(json.load(open('$SESSION_FILE')).get('preferred_handle',''))" 2>/dev/null || true)
            fi
        fi
        if [ -n "$SAVED_HANDLE" ]; then
            sed -i '' "s|preferred_handle: .*|preferred_handle: '$SAVED_HANDLE'|" "$CONFIG"
            echo "✓ Restored preferred handle: $SAVED_HANDLE"
        fi
    fi
fi

if [ "$NEEDS_LOGIN" = "true" ] && [ -t 0 ]; then
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  No iMessage login found — starting login...    │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    # Stop the bridge if running (otherwise it holds the DB lock)
    launchctl unload "$PLIST" 2>/dev/null || true
    (cd "$DATA_DIR" && "$BINARY" login -c "$CONFIG")
    echo ""
elif [ "$NEEDS_LOGIN" = "true" ]; then
    echo ""
    echo "  ⚠ No iMessage login found. Run interactively to log in:"
    echo "    $BINARY login -c $CONFIG"
    echo ""
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
echo "Waiting for bridge to start..."
for i in $(seq 1 15); do
    if grep -q "Bridge started" "$LOG_OUT" 2>/dev/null; then
        echo "✓ Bridge is running"
        break
    fi
    sleep 1
done

echo ""
echo "═══════════════════════════════════════════════"
echo "  Setup Complete"
echo "═══════════════════════════════════════════════"
echo ""
echo "  Logs:    tail -f $LOG_OUT"
echo "  Restart: launchctl kickstart -k gui/$(id -u)/$BUNDLE_ID"
echo "  Stop:    launchctl bootout gui/$(id -u)/$BUNDLE_ID"
echo ""
