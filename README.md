# mautrix-imessage

A Matrix-iMessage puppeting bridge. Send and receive iMessages from any Matrix client.

**Features**: text, images, video, audio, files, reactions/tapbacks, edits, unsends, typing indicators, read receipts, group chats, and contact name resolution.

## Install

### Prerequisites

macOS 14.2+ with:

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
!im login
```

Follow the prompts: Apple ID → password → 2FA code. The bridge registers with Apple's iMessage servers and connects.

### Chatting

To message someone:

```
!im resolve +15551234567
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
  ├── login.go               #   Apple ID + 2FA login flow
  ├── chatdb.go              #   chat.db backfill + contacts (macOS)
  ├── connector.go           #   bridge lifecycle
  └── ...
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
nac-validation/              # Local NAC via AppleAccount.framework
rustpush/                    # OpenBubbles/rustpush (vendored)
imessage/                    # macOS chat.db + Contacts reader
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
