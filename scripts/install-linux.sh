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
FIRST_RUN=false
mkdir -p "$DATA_DIR"
if [ -f "$CONFIG" ]; then
    echo "✓ Config already exists at $CONFIG"
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

    # Generate example config, then patch in user values
    "$BINARY" -c "$CONFIG" -e 2>/dev/null
    echo "✓ Generated config"

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
    # iMessage CloudKit chats can have tens of thousands of messages.
    # Deliver all history in one forward batch to avoid DAG fragmentation.
    sed -i 's/max_initial_messages: [0-9]*/max_initial_messages: 2147483647/' "$CONFIG"
    sed -i 's/max_catchup_messages: [0-9]*/max_catchup_messages: 5000/' "$CONFIG"
    sed -i 's/batch_size: [0-9]*/batch_size: 10000/' "$CONFIG"
    sed -i 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
    # Use 1s between batches — fast enough for backfill, prevents idle hot-loop
    sed -i 's/batch_delay: [0-9]*/batch_delay: 1/' "$CONFIG"
    echo "✓ Configured: $HS_ADDRESS, $HS_DOMAIN, $ADMIN_USER, $DB_TYPE"
fi

# ── Ensure backfill_source key exists in config ───────────────
if ! grep -q 'backfill_source:' "$CONFIG" 2>/dev/null; then
    sed -i '/cloudkit_backfill:/a\    backfill_source: cloudkit' "$CONFIG"
fi

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
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  Register with your homeserver:                 │"
    echo "│                                                 │"
    echo "│  Add to homeserver.yaml:                        │"
    echo "│    app_service_config_files:                    │"
    echo "│      - $REG_PATH"
    echo "│                                                 │"
    echo "│  Then restart your homeserver.                  │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    read -p "Press Enter once your homeserver is restarted..."
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
else
    # sqlite3 not available — can't verify DB has logins, assume login needed
    NEEDS_LOGIN=true
fi

# Require re-login if keychain trust-circle state is missing.
# This catches upgrades from pre-keychain versions where the device-passcode
# step was never run. If trustedpeers.plist exists with a user_identity, the
# keychain was joined successfully and any transient PCS errors are harmless.
SESSION_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/mautrix-imessage"
FORCE_CLEAR_STATE=false
if [ "$NEEDS_LOGIN" = "false" ]; then
    HAS_CLIQUE=false
    for _tp in "$SESSION_DIR"/trustedpeers*.plist; do
        if [ -f "$_tp" ] && grep -q "<key>userIdentity</key>\|<key>user_identity</key>" "$_tp" 2>/dev/null; then
            HAS_CLIQUE=true
            break
        fi
    done

    if [ "$HAS_CLIQUE" != "true" ]; then
        echo "⚠ Existing login found, but keychain trust-circle is not initialized."
        echo "  Forcing fresh login so device-passcode step can run."
        NEEDS_LOGIN=true
        FORCE_CLEAR_STATE=true
    fi
fi

if [ "$NEEDS_LOGIN" = "true" ]; then
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  No valid iMessage login found — starting login │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    # Stop the bridge if running (otherwise it holds the DB lock)
    if systemctl --user is-active mautrix-imessage >/dev/null 2>&1; then
        systemctl --user stop mautrix-imessage
    elif systemctl is-active mautrix-imessage >/dev/null 2>&1; then
        systemctl stop mautrix-imessage
    fi

    if [ "${FORCE_CLEAR_STATE:-false}" = "true" ]; then
        echo "Clearing stale local state before login..."
        rm -f "$DB_URI" "$DB_URI-wal" "$DB_URI-shm"
        rm -f "$SESSION_DIR/session.json" "$SESSION_DIR/identity.plist" "$SESSION_DIR"/trustedpeers*.plist
    fi

    # Run login from DATA_DIR so that relative paths (state/anisette/)
    # resolve to the same location as when systemd runs the bridge.
    (cd "$DATA_DIR" && "$BINARY" login -c "$CONFIG")
    echo ""
fi

# ── Contact source / CardDAV setup ───────────────────────────
# Skip if login just ran — the login flow already asked about contact source.
# On re-runs with an existing session, allow reconfiguration.
# Reads and writes per-user metadata in the bridge DB (not config.yaml).
if [ -t 0 ] && [ "$NEEDS_LOGIN" = "false" ] && command -v sqlite3 >/dev/null 2>&1 && [ -n "${DB_URI:-}" ] && [ -f "${DB_URI:-}" ]; then
    CURRENT_CARDDAV_EMAIL=$(sqlite3 "$DB_URI" "SELECT coalesce(json_extract(metadata, '$.CardDAVEmail'), '') FROM user_login LIMIT 1;" 2>/dev/null || true)
    CONFIGURE_CARDDAV=false

    if [ -n "$CURRENT_CARDDAV_EMAIL" ]; then
        echo ""
        echo "Contact source: External CardDAV ($CURRENT_CARDDAV_EMAIL)"
        read -p "Change contact provider? [y/N]: " CHANGE_CONTACTS
        case "$CHANGE_CONTACTS" in
            [yY]*) CONFIGURE_CARDDAV=true ;;
        esac
    else
        echo ""
        echo "Contact source (for resolving names in chats):"
        echo "  1) iCloud (default — uses your Apple ID)"
        echo "  2) Google Contacts (requires app password)"
        echo "  3) Fastmail"
        echo "  4) Nextcloud"
        echo "  5) Other CardDAV server"
        read -p "Choice [1]: " CONTACT_CHOICE
        CONTACT_CHOICE="${CONTACT_CHOICE:-1}"
        if [ "$CONTACT_CHOICE" != "1" ]; then
            CONFIGURE_CARDDAV=true
        fi
    fi

    if [ "$CONFIGURE_CARDDAV" = true ]; then
        if [ -n "$CURRENT_CARDDAV_EMAIL" ]; then
            echo ""
            echo "  1) iCloud (remove external CardDAV)"
            echo "  2) Google Contacts (requires app password)"
            echo "  3) Fastmail"
            echo "  4) Nextcloud"
            echo "  5) Other CardDAV server"
            read -p "Choice: " CONTACT_CHOICE
        fi

        CARDDAV_EMAIL=""
        CARDDAV_PASSWORD=""
        CARDDAV_USERNAME=""
        CARDDAV_URL=""

        if [ "${CONTACT_CHOICE:-}" = "1" ]; then
            sqlite3 "$DB_URI" "UPDATE user_login SET metadata = json_set(metadata, '$.CardDAVEmail', '', '$.CardDAVURL', '', '$.CardDAVUsername', '', '$.CardDAVPasswordEncrypted', '');"
            echo "✓ Switched to iCloud contacts"
        elif [ -n "${CONTACT_CHOICE:-}" ]; then
            read -p "Email address: " CARDDAV_EMAIL
            if [ -z "$CARDDAV_EMAIL" ]; then
                echo "ERROR: Email is required." >&2
                exit 1
            fi

            case "$CONTACT_CHOICE" in
                2)
                    CARDDAV_URL="https://www.googleapis.com/carddav/v1/principals/$CARDDAV_EMAIL/lists/default/"
                    echo "  Note: Use a Google App Password, without spaces (https://myaccount.google.com/apppasswords)"
                    ;;
                3)
                    CARDDAV_URL="https://carddav.fastmail.com/dav/addressbooks/user/$CARDDAV_EMAIL/Default/"
                    echo "  Note: Use a Fastmail App Password (Settings → Privacy & Security → App Passwords)"
                    ;;
                4)
                    read -p "Nextcloud server URL (e.g. https://cloud.example.com): " NC_SERVER
                    NC_SERVER="${NC_SERVER%/}"
                    CARDDAV_URL="$NC_SERVER/remote.php/dav"
                    ;;
                5)
                    read -p "CardDAV server URL: " CARDDAV_URL
                    if [ -z "$CARDDAV_URL" ]; then
                        echo "ERROR: URL is required." >&2
                        exit 1
                    fi
                    ;;
            esac

            read -p "Username (leave empty to use email): " CARDDAV_USERNAME
            read -s -p "App password: " CARDDAV_PASSWORD
            echo ""
            if [ -z "$CARDDAV_PASSWORD" ]; then
                echo "ERROR: Password is required." >&2
                exit 1
            fi

            CARDDAV_ARGS="--email $CARDDAV_EMAIL --password $CARDDAV_PASSWORD --url $CARDDAV_URL"
            if [ -n "$CARDDAV_USERNAME" ]; then
                CARDDAV_ARGS="$CARDDAV_ARGS --username $CARDDAV_USERNAME"
            fi
            CARDDAV_JSON=$("$BINARY" carddav-setup $CARDDAV_ARGS 2>/dev/null) || CARDDAV_JSON=""

            if [ -z "$CARDDAV_JSON" ]; then
                echo "⚠  CardDAV setup failed. You can reconfigure with 'set-carddav' in the bridge bot."
            else
                CARDDAV_RESOLVED_URL=$(echo "$CARDDAV_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['url'])")
                CARDDAV_ENC=$(echo "$CARDDAV_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['password_encrypted'])")
                EFFECTIVE_USERNAME="${CARDDAV_USERNAME:-$CARDDAV_EMAIL}"
                sqlite3 "$DB_URI" "UPDATE user_login SET metadata = json_set(metadata, '$.CardDAVEmail', '$CARDDAV_EMAIL', '$.CardDAVURL', '$CARDDAV_RESOLVED_URL', '$.CardDAVUsername', '$EFFECTIVE_USERNAME', '$.CardDAVPasswordEncrypted', '$CARDDAV_ENC');"
                echo "✓ CardDAV configured: $CARDDAV_EMAIL → $CARDDAV_RESOLVED_URL"
            fi
        fi
    fi
fi

# ── Optional shell shortcuts (asked before preferred handle so the
#    handle prompt remains the last interactive step) ─────────────
# Detect existing systemd scope from installed unit files. If neither
# scope has the unit yet (first-time install before systemd setup),
# default to --user (the common path for non-root installs).
_SHORTCUT_SYSCTL=""
_SHORTCUT_JCTL=""
if systemctl --user list-unit-files mautrix-imessage.service 2>/dev/null | grep -q mautrix-imessage; then
    _SHORTCUT_SYSCTL="systemctl --user"
    _SHORTCUT_JCTL="journalctl --user"
elif systemctl list-unit-files mautrix-imessage.service 2>/dev/null | grep -q mautrix-imessage; then
    _SHORTCUT_SYSCTL="systemctl"
    _SHORTCUT_JCTL="journalctl"
else
    _SHORTCUT_SYSCTL="systemctl --user"
    _SHORTCUT_JCTL="journalctl --user"
fi

echo ""
echo "Want easy commands you can type from any terminal to control the bridge?"
echo "  start-imessage     stop-imessage     restart-imessage     imessage-log"
read -r -p "Add them? [y/N]: " _shortcut_ans
case "$_shortcut_ans" in
    [yY]|[yY][eE][sS])
        case "$SHELL" in
            */zsh)  RC_FILE="$HOME/.zshrc" ;;
            */bash) RC_FILE="$HOME/.bashrc" ;;
            *)      RC_FILE="" ;;
        esac
        if [ -z "$RC_FILE" ]; then
            echo "  Couldn't detect your shell from \$SHELL ($SHELL) — skipping. (Bash and Zsh are supported.)"
        else
            MARKER_START="# >>> mautrix-imessage shortcuts (managed) >>>"
            MARKER_END="# <<< mautrix-imessage shortcuts (managed) <<<"
            if [ -f "$RC_FILE" ] && grep -qF "$MARKER_START" "$RC_FILE"; then
                awk -v s="$MARKER_START" -v e="$MARKER_END" '
                    $0 == s { skip = 1; next }
                    $0 == e { skip = 0; next }
                    !skip   { print }
                ' "$RC_FILE" > "$RC_FILE.tmp" && mv "$RC_FILE.tmp" "$RC_FILE"
            fi
            cat >> "$RC_FILE" <<EOF
$MARKER_START
alias start-imessage='$_SHORTCUT_SYSCTL start mautrix-imessage'
alias stop-imessage='$_SHORTCUT_SYSCTL stop mautrix-imessage'
alias restart-imessage='$_SHORTCUT_SYSCTL restart mautrix-imessage'
alias imessage-log='$_SHORTCUT_JCTL -u mautrix-imessage -f'
$MARKER_END
EOF
            echo "  ✓ Shortcuts added. Open a new terminal (or run \`source $RC_FILE\` here) and you can type:"
            echo "      start-imessage   stop-imessage   restart-imessage   imessage-log"
        fi
        ;;
    *)
        # User declined. If a previous run installed shortcuts, treat the
        # decline as "remove them" so the rc file matches the user's choice.
        case "$SHELL" in
            */zsh)  RC_FILE="$HOME/.zshrc" ;;
            */bash) RC_FILE="$HOME/.bashrc" ;;
            *)      RC_FILE="" ;;
        esac
        MARKER_START="# >>> mautrix-imessage shortcuts (managed) >>>"
        MARKER_END="# <<< mautrix-imessage shortcuts (managed) <<<"
        if [ -n "$RC_FILE" ] && [ -f "$RC_FILE" ] && grep -qF "$MARKER_START" "$RC_FILE"; then
            awk -v s="$MARKER_START" -v e="$MARKER_END" '
                $0 == s { skip = 1; next }
                $0 == e { skip = 0; next }
                !skip   { print }
            ' "$RC_FILE" > "$RC_FILE.tmp" && mv "$RC_FILE.tmp" "$RC_FILE"
            echo "  Removed previously-installed shortcuts from $RC_FILE."
        else
            echo "  Skipped — re-run this installer to add them later."
        fi
        ;;
esac
echo ""

# ── Preferred handle (runs every time, can reconfigure) ────────
CURRENT_HANDLE=$(grep 'preferred_handle:' "$CONFIG" 2>/dev/null | head -1 | sed "s/.*preferred_handle: *//;s/['\"]//g" | tr -d ' ' || true)

# Try to recover from backups if not set in config
if [ -z "$CURRENT_HANDLE" ]; then
    if command -v sqlite3 >/dev/null 2>&1 && [ -n "${DB_URI:-}" ] && [ -f "${DB_URI:-}" ]; then
        CURRENT_HANDLE=$(sqlite3 "$DB_URI" "SELECT json_extract(metadata, '$.preferred_handle') FROM user_login LIMIT 1;" 2>/dev/null || true)
    fi
    if [ -z "$CURRENT_HANDLE" ] && [ -f "$SESSION_DIR/session.json" ] && command -v python3 >/dev/null 2>&1; then
        CURRENT_HANDLE=$(python3 -c "import json; print(json.load(open('$SESSION_DIR/session.json')).get('preferred_handle',''))" 2>/dev/null || true)
    fi
fi

# Skip interactive prompt if login just ran (login flow already asked)
if [ -t 0 ] && [ "$NEEDS_LOGIN" = "false" ]; then
    # Get available handles from session state (available after login)
    AVAILABLE_HANDLES=$("$BINARY" list-handles 2>/dev/null | grep -E '^(tel:|mailto:)' || true)
    if [ -n "$AVAILABLE_HANDLES" ]; then
        echo ""
        echo "Preferred handle (your iMessage sender address):"
        i=1
        declare -a HANDLE_LIST=()
        while IFS= read -r h; do
            MARKER=""
            if [ "$h" = "$CURRENT_HANDLE" ]; then
                MARKER=" (current)"
            fi
            echo "  $i) $h$MARKER"
            HANDLE_LIST+=("$h")
            i=$((i + 1))
        done <<< "$AVAILABLE_HANDLES"

        if [ -n "$CURRENT_HANDLE" ]; then
            read -p "Choice [keep current]: " HANDLE_CHOICE
        else
            read -p "Choice [1]: " HANDLE_CHOICE
        fi

        if [ -n "$HANDLE_CHOICE" ]; then
            if [ "$HANDLE_CHOICE" -ge 1 ] 2>/dev/null && [ "$HANDLE_CHOICE" -le "${#HANDLE_LIST[@]}" ] 2>/dev/null; then
                CURRENT_HANDLE="${HANDLE_LIST[$((HANDLE_CHOICE - 1))]}"
            fi
        elif [ -z "$CURRENT_HANDLE" ] && [ ${#HANDLE_LIST[@]} -gt 0 ]; then
            CURRENT_HANDLE="${HANDLE_LIST[0]}"
        fi
    elif [ -n "$CURRENT_HANDLE" ]; then
        echo ""
        echo "Preferred handle: $CURRENT_HANDLE"
        read -p "New handle, or Enter to keep current: " NEW_HANDLE
        if [ -n "$NEW_HANDLE" ]; then
            CURRENT_HANDLE="$NEW_HANDLE"
        fi
    fi
fi

# Write preferred handle to DB
if [ -n "${CURRENT_HANDLE:-}" ]; then
    if command -v sqlite3 >/dev/null 2>&1 && [ -n "${DB_URI:-}" ] && [ -f "${DB_URI:-}" ]; then
        sqlite3 "$DB_URI" "UPDATE user_login SET metadata = json_set(metadata, '$.preferred_handle', '$CURRENT_HANDLE');"
    fi
    echo "✓ Preferred handle: $CURRENT_HANDLE"
fi

# ── Install ffmpeg if available (needed for video transcoding) ─
echo "Checking for ffmpeg..."
if ! command -v ffmpeg >/dev/null 2>&1; then
    if command -v apt >/dev/null 2>&1; then
        apt install -y ffmpeg 2>/dev/null || true
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y ffmpeg 2>/dev/null || true
    elif command -v pacman >/dev/null 2>&1; then
        pacman -S --noconfirm ffmpeg 2>/dev/null || true
    elif command -v zypper >/dev/null 2>&1; then
        zypper install -y ffmpeg 2>/dev/null || true
    elif command -v apk >/dev/null 2>&1; then
        apk add ffmpeg 2>/dev/null || true
    fi
fi

# ── Ensure disable_facetime key exists in config ──────────────
if ! grep -q 'disable_facetime:' "$CONFIG" 2>/dev/null; then
    sed -i '/video_transcoding:/a\    disable_facetime: false' "$CONFIG"
fi

# ── Disable FaceTime Bridge (use native Apple FT instead) ────────
CURRENT_DISABLE_FT=$(grep 'disable_facetime:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*disable_facetime: *//' || true)
if [ -n "${DISABLE_FACETIME:-}" ]; then
    case "$DISABLE_FACETIME" in
        1|true|TRUE|yes|YES)
            sed -i "s/disable_facetime: .*/disable_facetime: true/" "$CONFIG"
            echo "✓ FaceTime Bridge disabled (DISABLE_FACETIME env)"
            ;;
        *)
            sed -i "s/disable_facetime: .*/disable_facetime: false/" "$CONFIG"
            echo "✓ FaceTime Bridge enabled (DISABLE_FACETIME env)"
            ;;
    esac
elif [ -t 0 ]; then
    echo ""
    echo "FaceTime Bridge:"
    echo "  If you have an Apple device that already handles FaceTime, the"
    echo "  bridge's FT wrapper just clutters your chat. Disable it to skip"
    echo "  !im facetime commands and inbound FT notices."
    echo ""
    if [ "$CURRENT_DISABLE_FT" = "true" ]; then
        read -p "Enable FaceTime Bridge? [y/N]: " EN_FT
        case "$EN_FT" in
            [yY]*)
                sed -i "s/disable_facetime: .*/disable_facetime: false/" "$CONFIG"
                echo "✓ FaceTime Bridge enabled"
                ;;
            *)
                echo "✓ FaceTime Bridge disabled"
                ;;
        esac
    else
        read -p "Enable FaceTime Bridge? [Y/n]: " EN_FT
        case "$EN_FT" in
            [nN]*)
                sed -i "s/disable_facetime: .*/disable_facetime: true/" "$CONFIG"
                echo "✓ FaceTime Bridge disabled"
                ;;
            *)
                echo "✓ FaceTime Bridge enabled"
                ;;
        esac
    fi
fi

# ── Ensure statuskit_notifications key exists in config ─────
if ! grep -q 'statuskit_notifications:' "$CONFIG" 2>/dev/null; then
    sed -i '/disable_facetime:/a\    statuskit_notifications: true' "$CONFIG"
fi

# ── StatusKit notifications (iOS 18 Focus / DND inline notices) ───
CURRENT_STATUSKIT_NOTIF=$(grep 'statuskit_notifications:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*statuskit_notifications: *//' || true)
if [ -n "${STATUSKIT_NOTIFICATIONS:-}" ]; then
    case "$STATUSKIT_NOTIFICATIONS" in
        1|true|TRUE|yes|YES)
            sed -i "s/statuskit_notifications: .*/statuskit_notifications: true/" "$CONFIG"
            echo "✓ StatusKit notifications enabled (STATUSKIT_NOTIFICATIONS env)"
            ;;
        *)
            sed -i "s/statuskit_notifications: .*/statuskit_notifications: false/" "$CONFIG"
            echo "✓ StatusKit notifications disabled (STATUSKIT_NOTIFICATIONS env)"
            ;;
    esac
elif [ -t 0 ]; then
    echo ""
    echo "StatusKit notifications:"
    echo "  When a contact enables iOS 18 Focus or Do Not Disturb on their"
    echo "  iPhone, the bridge can post a silent notice in the DM portal"
    echo "  (\"🔕 Name has notifications silenced (Do Not Disturb).\") and"
    echo "  update Matrix ghost presence — the same affordance Apple's"
    echo "  Messages app shows in-conversation. Disabling keeps the"
    echo "  StatusKit registration intact but suppresses the notices."
    echo ""
    echo "  Note: when a notification is posted, the destination chat will"
    echo "  be unarchived. This is a limitation external to the bridge."
    echo ""
    if [ "$CURRENT_STATUSKIT_NOTIF" = "false" ]; then
        read -p "Enable StatusKit notifications? [y/N]: " EN_SK
        case "$EN_SK" in
            [yY]*)
                sed -i "s/statuskit_notifications: .*/statuskit_notifications: true/" "$CONFIG"
                echo "✓ StatusKit notifications enabled"
                ;;
            *)
                echo "✓ StatusKit notifications disabled"
                ;;
        esac
    else
        read -p "Enable StatusKit notifications? [Y/n]: " EN_SK
        case "$EN_SK" in
            [nN]*)
                sed -i "s/statuskit_notifications: .*/statuskit_notifications: false/" "$CONFIG"
                echo "✓ StatusKit notifications disabled"
                ;;
            *)
                echo "✓ StatusKit notifications enabled"
                ;;
        esac
    fi
fi

# ── Install libheif if available (needed for HEIC conversion) ─
echo "Checking for libheif..."
if ! ldconfig -p 2>/dev/null | grep -q libheif || ! command -v heif-convert >/dev/null 2>&1; then
    if command -v apt >/dev/null 2>&1; then
        dpkg -s libheif-dev >/dev/null 2>&1 || apt install -y libheif-dev 2>/dev/null || true
    elif command -v dnf >/dev/null 2>&1; then
        rpm -q libheif-devel >/dev/null 2>&1 || dnf install -y libheif-devel 2>/dev/null || true
    elif command -v pacman >/dev/null 2>&1; then
        pacman -Qi libheif >/dev/null 2>&1 || pacman -S --noconfirm libheif 2>/dev/null || true
    elif command -v zypper >/dev/null 2>&1; then
        rpm -q libheif-devel >/dev/null 2>&1 || zypper install -y libheif-devel 2>/dev/null || true
    elif command -v apk >/dev/null 2>&1; then
        apk info -e libheif-dev >/dev/null 2>&1 || apk add libheif-dev 2>/dev/null || true
    fi
fi

# ── Install systemd service (optional) ────────────────────────
# Detect whether systemd user sessions work. In containers (LXC) or when
# running as root, the user instance is often unavailable — fall back to a
# system-level service in that case.
USER_SERVICE_FILE="$HOME/.config/systemd/user/mautrix-imessage.service"
SYSTEM_SERVICE_FILE="/etc/systemd/system/mautrix-imessage.service"

if command -v systemctl >/dev/null 2>&1; then
    if systemctl --user status >/dev/null 2>&1; then
        SYSTEMD_MODE="user"
        SERVICE_FILE="$USER_SERVICE_FILE"
    else
        SYSTEMD_MODE="system"
        SERVICE_FILE="$SYSTEM_SERVICE_FILE"
    fi
else
    SYSTEMD_MODE="none"
    SERVICE_FILE=""
fi

install_systemd_user() {
    # Enable lingering so user services survive SSH session closures
    if command -v loginctl >/dev/null 2>&1 && [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]; then
        loginctl enable-linger "$USER" 2>/dev/null || true
    fi
    mkdir -p "$(dirname "$USER_SERVICE_FILE")"
    cat > "$USER_SERVICE_FILE" << EOF
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
}

install_systemd_system() {
    cat > "$SYSTEM_SERVICE_FILE" << EOF
[Unit]
Description=mautrix-imessage bridge
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=$(dirname "$BINARY")
ExecStart=$BINARY -c $CONFIG
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable mautrix-imessage
}

if [ "$SYSTEMD_MODE" = "user" ]; then
    if [ -f "$USER_SERVICE_FILE" ]; then
        install_systemd_user
        systemctl --user restart mautrix-imessage
        echo "✓ Bridge restarted"
    else
        echo ""
        read -p "Install as a systemd user service? [Y/n] " answer
        case "$answer" in
            [nN]*) ;;
            *)     install_systemd_user
                   systemctl --user start mautrix-imessage
                   echo "✓ Bridge started (systemd user service installed)" ;;
        esac
    fi
elif [ "$SYSTEMD_MODE" = "system" ]; then
    if [ -f "$SYSTEM_SERVICE_FILE" ]; then
        install_systemd_system
        systemctl restart mautrix-imessage
        echo "✓ Bridge restarted"
    else
        echo ""
        echo "Note: systemd user session not available (container/root)."
        read -p "Install as a system-level systemd service? [Y/n] " answer
        case "$answer" in
            [nN]*) ;;
            *)     install_systemd_system
                   systemctl start mautrix-imessage
                   echo "✓ Bridge started (system service installed)" ;;
        esac
    fi
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
if [ "$SYSTEMD_MODE" = "user" ] && [ -f "$USER_SERVICE_FILE" ]; then
    echo "  Status:  systemctl --user status mautrix-imessage"
    echo "  Logs:    journalctl --user -u mautrix-imessage -f"
    echo "  Stop:    systemctl --user stop mautrix-imessage"
    echo "  Restart: systemctl --user restart mautrix-imessage"
elif [ "$SYSTEMD_MODE" = "system" ] && [ -f "$SYSTEM_SERVICE_FILE" ]; then
    echo "  Status:  systemctl status mautrix-imessage"
    echo "  Logs:    journalctl -u mautrix-imessage -f"
    echo "  Stop:    systemctl stop mautrix-imessage"
    echo "  Restart: systemctl restart mautrix-imessage"
else
    echo "  Run manually:"
    echo "    cd $(dirname "$CONFIG") && $BINARY -c $CONFIG"
fi
echo ""

