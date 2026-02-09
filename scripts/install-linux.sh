#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
REGISTRATION="$DATA_DIR/registration.yaml"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup (Standalone · Linux)"
echo "═══════════════════════════════════════════════"
echo ""

# ── Generate config ───────────────────────────────────────────
mkdir -p "$DATA_DIR"
if [ -f "$CONFIG" ]; then
    echo "✓ Config already exists at $CONFIG"
else
    echo "Generating config..."
    "$BINARY" -g -c "$CONFIG" -r "$REGISTRATION"
    echo "✓ Config generated at $CONFIG"
fi

# ── Collect homeserver details ────────────────────────────────
if ! grep -q "address:" "$CONFIG" 2>/dev/null || grep -q "address: https://example.com" "$CONFIG" 2>/dev/null; then
    echo ""
    read -p "Homeserver URL (e.g. https://matrix.example.com): " HS_URL
    read -p "Homeserver domain (e.g. example.com): " HS_DOMAIN
    read -p "Your Matrix ID (e.g. @you:example.com): " MATRIX_ID

    # Patch config
    sed -i "s|address: .*|address: $HS_URL|" "$CONFIG"
    sed -i "s|domain: .*|domain: $HS_DOMAIN|" "$CONFIG"

    echo "✓ Config updated with homeserver details"
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  Register the bridge with your homeserver       │"
    echo "│                                                 │"
    echo "│  Add to your homeserver's config:               │"
    echo "│                                                 │"
    echo "│  app_service_config_files:                      │"
    echo "│    - $REGISTRATION"
    echo "│                                                 │"
    echo "│  Then restart your homeserver.                  │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    read -p "Press Enter once your homeserver is restarted..."
fi

# ── Install systemd service (optional) ────────────────────────
SERVICE_FILE="$HOME/.config/systemd/user/mautrix-imessage.service"

install_systemd() {
    mkdir -p "$(dirname "$SERVICE_FILE")"
    cat > "$SERVICE_FILE" << EOF
[Unit]
Description=mautrix-imessage bridge
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
    systemctl --user start mautrix-imessage
    echo "✓ Bridge started (systemd user service installed)"
}

echo ""
if command -v systemctl >/dev/null 2>&1 && systemctl --user status >/dev/null 2>&1; then
    read -p "Install as a systemd user service? [Y/n] " answer
    case "$answer" in
        [nN]*) ;;
        *)     install_systemd ;;
    esac
fi

echo ""
echo "═══════════════════════════════════════════════"
echo "  Setup Complete"
echo "═══════════════════════════════════════════════"
echo ""
echo "  Binary: $BINARY"
echo "  Config: $CONFIG"
echo "  Registration: $REGISTRATION"
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
echo "  Next: DM the bridge bot and use the 'External Key'"
echo "  login flow with your hardware key."
echo ""
