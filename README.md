# mautrix-imessage

A Matrix-iMessage puppeting bridge. Send and receive iMessages from any Matrix client.

**Features**: text, images, video, audio, files, reactions/tapbacks, edits, unsends, typing indicators, read receipts, group chats, and contact name resolution.

## Install

### Prerequisites

macOS 14.2+ with:

- **Signed into iCloud** on the Mac running the bridge (System Settings → Apple ID). This is required for Apple to recognize the device as trusted and allow login without 2FA prompts.
- Full Disk Access granted to the bridge app (for chat.db backfill — prompted on first run).

```bash
brew install go rust libolm protobuf
```

A running Matrix homeserver ([Synapse](https://element-hq.github.io/synapse/), etc).

### Setup

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install
```

This builds the bridge, asks three questions (homeserver URL, domain, your Matrix ID), generates config files, and starts the bridge as a LaunchAgent.

The installer will pause and ask you to register the bridge with your homeserver — it tells you exactly what to add to `homeserver.yaml`.

Once running, DM `@imessagebot:yourdomain` in your Matrix client and send:

```
login
```

Follow the prompts: Apple ID → password. If the Mac is signed into iCloud with the same Apple ID, login completes without 2FA. The bridge registers with Apple's iMessage servers and connects.

> **Note:** In a DM with the bot, commands don't need a prefix. In a regular room, use `!im login`, `!im help`, etc.

### Setup with Beeper

If you use [Beeper](https://www.beeper.com) instead of a self-hosted homeserver, you can skip the Synapse setup and use [`bbctl`](https://github.com/beeper/bridge-manager) to connect the bridge to Beeper's cloud homeserver:

1. Install `bbctl`:
   ```bash
   # Download from https://github.com/beeper/bridge-manager/releases
   chmod +x bbctl && sudo mv bbctl /usr/local/bin/
   ```

2. Log in to your Beeper account:
   ```bash
   bbctl login
   ```

3. Build and install:
   ```bash
   make install-beeper
   ```

This runs `bbctl config --type bridgev2 sh-imessage` to generate a config that talks to Beeper's homeserver, then installs the same LaunchAgent as the regular path. No registration file, no `homeserver.yaml` edits.

Once running, DM `@sh-imessagebot:YOUR_BEEPER_DOMAIN` in Beeper and send `login`.

### SMS Forwarding

To bridge SMS (green bubble) messages, enable the bridge device on your iPhone:

**Settings → Messages → Text Message Forwarding** → toggle on the new device.

Each bridge login registers as a separate device, so you may see multiple entries — enable the latest one.

### Chatting

Once logged in, incoming iMessages automatically create Matrix rooms — no setup needed per conversation. If you grant Full Disk Access (System Settings → Privacy & Security), existing conversations from Messages.app are also synced.

To start a **new** conversation with someone who hasn't messaged you:

```
resolve +15551234567
```

This creates a portal room. Messages you send there are delivered as iMessages; replies appear in the room.

## How it works

The bridge connects directly to Apple's iMessage servers using the [rustpush](https://github.com/OpenBubbles/rustpush) library with local NAC validation (no SIP bypass, no relay server). On macOS with Full Disk Access, it also reads `chat.db` for message history backfill and contact name resolution from Contacts.app.

```
Matrix client ←→ Synapse
                    ↓ appservice
              mautrix-imessage
                    ↓
              rustpush (Rust FFI)
                    ↓
              Apple IDS / APNs
```

### Real-time vs. backfill

**Real-time messages** are handled by rustpush — incoming and outgoing iMessages flow through Apple's push notification service (APNs) and appear in Matrix immediately.

**Backfill** fills in anything rustpush misses by reading the local macOS `chat.db`. On startup, the bridge creates portals for all chats with activity in the last 7 days and backfills their messages. A continuous health check then runs every 5 minutes:

1. Query chat.db for all message GUIDs in the last 24 hours
2. Compare against message IDs already bridged (set-diff by GUID — no timestamps)
3. If any messages are missing, **nuke the portal** (delete bridge DB entries + Matrix room) and **re-create it from scratch** with a full chronological backfill

This nuke-and-resync approach guarantees messages always appear in correct chronological order in the Matrix timeline, since standard Matrix APIs can only append events — they can't insert into the middle of history.

The system is idempotent and safe to run repeatedly. If nothing is missing, the health check is a no-op.

## Management

```bash
# View logs
tail -f data/bridge.stdout.log

# Restart
launchctl stop com.lrhodin.mautrix-imessage

# Stop
launchctl unload ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist

# Remove
make uninstall
```

## Configuration

Config lives in `data/config.yaml` (generated during `make install`). To reconfigure:

```bash
rm -rf data
make install
```

Key options:

| Field | What it does |
|-------|-------------|
| `database.type` | `sqlite3-fk-wal` (default) or `postgres` |
| `encryption` | End-to-bridge encryption settings |
| `network.displayname_template` | Contact name format |
| `backfill.max_initial_messages` | Max messages per backfill (default 10000) |
| `backfill.max_catchup_messages` | Max messages for catch-up after restart |

## Development

```bash
make build      # Build .app bundle
make rust       # Build Rust library only
make bindings   # Regenerate Go FFI bindings (needs uniffi-bindgen-go)
make clean      # Remove build artifacts
```

### Source layout

```
cmd/mautrix-imessage/        # Entrypoint
pkg/connector/               # bridgev2 connector (unified)
  ├── client.go              #   send/receive/reactions/edits/typing
  │                          #   + periodic health check & backfill
  ├── login.go               #   Apple ID + 2FA login flow
  ├── chatdb.go              #   chat.db backfill + contacts (macOS)
  │                          #   + GUID set-diff for missing messages
  ├── ids.go                 #   identifier/portal ID conversion
  ├── connector.go           #   bridge lifecycle
  └── ...
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
nac-validation/              # Local NAC via AppleAccount.framework
rustpush/                    # OpenBubbles/rustpush (vendored)
imessage/                    # macOS chat.db + Contacts reader
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
