#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"

BRIDGE_NAME="${BRIDGE_NAME:-sh-imessage}"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"

# Where we build/cache bbctl
BBCTL_DIR="${BBCTL_DIR:-$HOME/.local/share/mautrix-imessage/bridge-manager}"
BBCTL_REPO="${BBCTL_REPO:-https://github.com/lrhodin/bridge-manager.git}"
BBCTL_BRANCH="${BBCTL_BRANCH:-add-imessage-v2}"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup (Beeper · Linux)"
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
fi

# ── Check bbctl login ────────────────────────────────────────
WHOAMI_CHECK=$("$BBCTL" whoami 2>&1 || true)
if echo "$WHOAMI_CHECK" | grep -qi "not logged in" || [ -z "$WHOAMI_CHECK" ]; then
    echo ""
    echo "Not logged into Beeper. Running bbctl login..."
    echo ""
    "$BBCTL" login
fi
WHOAMI=$("$BBCTL" whoami 2>&1 || true)
WHOAMI=$(echo "$WHOAMI" | head -1)
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
    sed -i "s|uri: file:mautrix-imessage.db|uri: file:$DATA_DIR/mautrix-imessage.db|" "$CONFIG"
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

if [ "$NEEDS_LOGIN" = "true" ] && [ -t 0 ]; then
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  No iMessage login found — starting login...    │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    # Stop the bridge if running (otherwise it holds the DB lock)
    if systemctl --user is-active mautrix-imessage >/dev/null 2>&1; then
        systemctl --user stop mautrix-imessage
    fi
    "$BINARY" login -c "$CONFIG"
    echo ""
elif [ "$NEEDS_LOGIN" = "true" ]; then
    echo ""
    echo "  ℹ No iMessage login found. Run interactively to log in:"
    echo "    $BINARY login -c $CONFIG"
    echo ""
fi

# ── Install / update systemd service ─────────────────────────
SERVICE_FILE="$HOME/.config/systemd/user/mautrix-imessage.service"

install_systemd() {
    mkdir -p "$(dirname "$SERVICE_FILE")"
    cat > "$SERVICE_FILE" << EOF
[Unit]
Description=mautrix-imessage bridge (Beeper)
After=network.target

[Service]
Type=simple
WorkingDirectory=$(dirname "$BINARY")
ExecStart=$BINARY -c $CONFIG
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    systemctl --user enable mautrix-imessage
}

if command -v systemctl >/dev/null 2>&1 && systemctl --user status >/dev/null 2>&1; then
    if [ -f "$SERVICE_FILE" ]; then
        # Update: rebuild service file (binary path may change), restart
        install_systemd
        systemctl --user restart mautrix-imessage
        echo "✓ Bridge restarted"
    elif [ -t 0 ]; then
        # Fresh install with TTY: ask
        echo ""
        read -p "Install as a systemd user service? [Y/n] " answer
        case "$answer" in
            [nN]*) ;;
            *)     install_systemd
                   systemctl --user start mautrix-imessage
                   echo "✓ Bridge started (systemd user service installed)" ;;
        esac
    else
        # Fresh install without TTY: install automatically
        install_systemd
        systemctl --user start mautrix-imessage
        echo "✓ Bridge started (systemd user service installed)"
    fi
fi

echo ""
echo "═══════════════════════════════════════════════"
echo "  Setup Complete"
echo "═══════════════════════════════════════════════"
echo ""
echo "  Binary: $BINARY"
echo "  Config: $CONFIG"
echo ""
if [ -f "$SERVICE_FILE" ]; then
    echo "  Status:  systemctl --user status mautrix-imessage"
    echo "  Logs:    journalctl --user -u mautrix-imessage -f"
    echo "  Stop:    systemctl --user stop mautrix-imessage"
    echo "  Restart: systemctl --user restart mautrix-imessage"
else
    echo "  Run manually:"
    echo "    cd $(dirname "$CONFIG") && $BINARY -c $CONFIG"
fi
echo ""
