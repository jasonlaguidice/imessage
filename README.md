# mautrix-imessage

A Matrix-iMessage puppeting bridge. Send and receive iMessages from any Matrix client.

Two connectors are included:

| | **mac** (chat.db) | **rustpush** (direct protocol) |
|---|---|---|
| How it works | Reads `~/Library/Messages/chat.db`, sends via AppleScript | Connects directly to Apple's iMessage servers |
| Requires | Full Disk Access | Apple ID login (via bridge bot) |
| Send messages | ✅ | ✅ |
| Receive messages | ✅ (polling, ~5s delay) | ✅ (real-time) |
| Reactions / tapbacks | Receive only | ✅ Send + receive |
| Edits / unsends | ❌ | ✅ Send + receive |
| Typing indicators | ❌ | ✅ Send + receive |
| Read receipts | Receive only | ✅ Send + receive |
| Attachments | ✅ | ✅ (MMCS) |
| Message history backfill | ✅ | ❌ |
| Contact name resolution | ✅ | ❌ |

Both can run simultaneously — rustpush handles real-time, mac handles backfill.

## Install

### Prerequisites

```bash
brew install go rust libolm protobuf
```

Also requires a running Matrix homeserver ([Synapse](https://element-hq.github.io/synapse/), etc).

### Setup

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install
```

This builds the bridge, asks you three questions (homeserver URL, domain, your Matrix ID), generates all config files, and starts the bridge as a LaunchAgent.

The installer will pause and ask you to register the bridge with your homeserver — it tells you exactly what to add to `homeserver.yaml`.

Once running, DM `@imessagebot:yourdomain` in your Matrix client and send:

```
!im login
```

Follow the prompts to enter your Apple ID, password, and 2FA code.

### Chatting

To message someone, send to the bridge bot:

```
!im resolve +15551234567
```

This creates a portal room. Messages you send there are delivered as iMessages, and replies appear in the room.

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

All config lives in `data/config.yaml` (generated during `make install`). To reconfigure from scratch:

```bash
rm -rf data
make install
```

Key options beyond the defaults:

| Field | What it does |
|-------|-------------|
| `network.platform` | `mac` (default) or `rustpush-local` |
| `database.type` | `sqlite3-fk-wal` (default) or `postgres` |
| `encryption` | End-to-bridge encryption settings |

## Architecture

```
Matrix client ←→ Synapse
                    ↓ appservice
              mautrix-imessage
                    │
        ┌───────────┴───────────┐
        │                       │
  mac connector          rustpush connector
  (chat.db + AppleScript) (Apple IDS/APNs)
                                │
                         local NAC validation
                         (AppleAccount.framework)
```

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
pkg/connector/               # bridgev2 connector
  ├── rustpush_client.go     #   rustpush send/receive/reactions/edits
  ├── rustpush_login.go      #   Apple ID + 2FA login flow
  ├── client.go              #   mac connector client
  ├── handleimessage.go      #   incoming message routing
  ├── connector.go           #   platform routing + dual-connector dedup
  └── ...
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
nac-validation/              # Local NAC via AAAbsintheContext
rustpush/                    # OpenBubbles/rustpush (vendored)
imessage/                    # macOS chat.db + AppleScript + Contacts
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
