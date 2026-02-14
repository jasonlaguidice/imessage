#!/usr/bin/env bash
#
# Full bridge reset: delete Beeper registration, wipe ALL local state.
# You will need to re-login (2FA) after reset.
#
# Usage: make reset
#
set -euo pipefail

STATE_DIR="$HOME/.local/share/mautrix-imessage"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$STATE_DIR/bridge-manager/bbctl"

# ── Preflight checks ────────────────────────────────────────
if [ ! -x "$BBCTL" ]; then
    echo "ERROR: bbctl not found at $BBCTL"
    exit 1
fi

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    BUNDLE_ID="${1:-com.lrhodin.mautrix-imessage}"
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop mautrix-imessage 2>/dev/null || true
fi

# Verify it's actually stopped
sleep 1
if pgrep -f mautrix-imessage-v2 >/dev/null 2>&1; then
    echo "ERROR: bridge process still running after stop"
    exit 1
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
echo "Deleting bridge registration from Beeper..."

# bbctl delete has an interactive confirmation prompt that requires a TTY.
# Use tmux to provide one, then send 'y' + Enter.
if ! command -v tmux >/dev/null 2>&1; then
    echo "ERROR: tmux is required for bbctl delete (interactive prompt)"
    exit 1
fi

TMUX_SESSION="_bbctl_del"
BBCTL_LOG=$(mktemp)
tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
tmux new-session -d -s "$TMUX_SESSION" "$BBCTL delete $BRIDGE_NAME 2>&1 | tee $BBCTL_LOG; echo EXIT_CODE=\$? >> $BBCTL_LOG; sleep 2"
sleep 2
tmux send-keys -t "$TMUX_SESSION" 'y' Enter

# Wait for completion
for i in $(seq 1 20); do
    if ! tmux has-session -t "$TMUX_SESSION" 2>/dev/null; then break; fi
    sleep 1
done
tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true

# Check result
if grep -q "EXIT_CODE=0" "$BBCTL_LOG" 2>/dev/null; then
    echo "  ✓ Server-side registration deleted"
elif grep -qi "not found\|not registered\|no bridge" "$BBCTL_LOG" 2>/dev/null; then
    echo "  ✓ Bridge was not registered (already clean)"
else
    echo "ERROR: bbctl delete may have failed. Log:"
    cat "$BBCTL_LOG"
    rm -f "$BBCTL_LOG"
    exit 1
fi
rm -f "$BBCTL_LOG"

# ── Wipe EVERYTHING ─────────────────────────────────────────
echo "Wiping all state in $STATE_DIR/ ..."
# Keep bridge-manager (bbctl binary) — it's just a build cache
find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" -exec rm -rf {} +

# Verify it's actually clean
REMAINING=$(find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" | wc -l)
if [ "$REMAINING" -ne 0 ]; then
    echo "ERROR: state directory not fully cleaned:"
    ls -la "$STATE_DIR/"
    exit 1
fi

echo ""
echo "✓ Bridge fully reset."
echo "  All state wiped — you will need to re-login (2FA)."
echo ""
echo "  Run 'make install-beeper' to re-register, login, and start the bridge."
