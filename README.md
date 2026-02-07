# mautrix-imessage

A Matrix-iMessage puppeting bridge. Send and receive iMessages from any Matrix client.

**Features**: text, images, video, audio, files, reactions/tapbacks, edits, unsends, typing indicators, read receipts, group chats, and contact name resolution.

## Quick Start

macOS 14.2+ required. Sign into iCloud on the Mac running the bridge (Settings → Apple ID) — this lets Apple recognize the device so login works without 2FA prompts.

### With Beeper

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install-beeper
```

The installer handles everything: Homebrew, dependencies, building, Beeper login, config, and LaunchAgent setup. Once running, DM `@sh-imessagebot:beeper.local` in Beeper and send `login`.

### With a Self-Hosted Homeserver

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install
```

The installer auto-installs Homebrew and dependencies if needed, asks three questions (homeserver URL, domain, your Matrix ID), generates config files, and starts the bridge as a LaunchAgent. It will pause and tell you exactly what to add to your `homeserver.yaml` to register the bridge.

Once running, DM `@imessagebot:yourdomain` in your Matrix client and send `login`.

### Login

Follow the prompts: Apple ID → password. If the Mac is signed into iCloud with the same Apple ID, login completes without 2FA.

> **Tip:** In a DM with the bot, commands don't need a prefix. In a regular room, use `!im login`, `!im help`, etc.

### SMS Forwarding

To bridge SMS (green bubble) messages, enable forwarding on your iPhone:

**Settings → Messages → Text Message Forwarding** → toggle on the bridge device.

### Chatting

Incoming iMessages automatically create Matrix rooms. If Full Disk Access is granted, existing conversations from Messages.app are also synced.

To start a **new** conversation:

```
resolve +15551234567
```

This creates a portal room. Messages you send there are delivered as iMessages.

## How It Works

The bridge connects directly to Apple's iMessage servers using [rustpush](https://github.com/OpenBubbles/rustpush) with local NAC validation (no SIP bypass, no relay server). On macOS with Full Disk Access, it also reads `chat.db` for message history backfill and contact name resolution.

```mermaid
flowchart TB
    subgraph sh["🖥 Self-hosted · Your Mac"]
        HS[Homeserver] -- appservice --> Bridge1[mautrix-imessage]
        Bridge1 -- FFI --> RP1[rustpush]
    end
    subgraph bp["🖥 Beeper · Your Mac"]
        Bridge2[mautrix-imessage] -- FFI --> RP2[rustpush]
    end
    Client1[Matrix client] <--> HS
    Client2[Beeper app] <--> Beeper[Beeper cloud]
    Beeper -- websocket --> Bridge2
    RP1 <--> Apple[Apple IDS / APNs]
    RP2 <--> Apple

    style sh fill:#f0f4ff,stroke:#4a6fa5,stroke-width:2px,color:#1a1a2e
    style bp fill:#f0f4ff,stroke:#4a6fa5,stroke-width:2px,color:#1a1a2e
    style Apple fill:#1a1a2e,stroke:#1a1a2e,color:#fff
    style Beeper fill:#1a1a2e,stroke:#1a1a2e,color:#fff
    style Client1 fill:#fff,stroke:#999,color:#333
    style Client2 fill:#fff,stroke:#999,color:#333
    style HS fill:#e8f0fe,stroke:#4a6fa5,color:#1a1a2e
    style Bridge1 fill:#d4e4ff,stroke:#2c5aa0,stroke-width:2px,color:#1a1a2e
    style Bridge2 fill:#d4e4ff,stroke:#2c5aa0,stroke-width:2px,color:#1a1a2e
    style RP1 fill:#e8f0fe,stroke:#4a6fa5,color:#1a1a2e
    style RP2 fill:#e8f0fe,stroke:#4a6fa5,color:#1a1a2e
```

### Real-time and backfill

**Real-time messages** flow through Apple's push notification service (APNs) via rustpush and appear in Matrix immediately.

**Backfill** runs once on first login: the bridge reads the local macOS `chat.db` and creates portals for all chats with activity in the last `initial_sync_days` (default: 1 year, configurable). After that, everything is real-time only via rustpush.

## Management

```bash
# View logs
tail -f data/bridge.stdout.log

# Restart (auto-restarts via KeepAlive)
launchctl kickstart -k gui/$(id -u)/com.lrhodin.mautrix-imessage

# Stop until next login
launchctl bootout gui/$(id -u)/com.lrhodin.mautrix-imessage

# Uninstall
make uninstall
```

## Configuration

Config lives in `data/config.yaml` (generated during install). To reconfigure from scratch:

```bash
rm -rf data
make install    # or make install-beeper
```

Key options:

| Field | Default | What it does |
|-------|---------|-------------|
| `network.initial_sync_days` | `365` | How far back to backfill on first login |
| `network.displayname_template` | First/Last name | How bridged contacts appear in Matrix |
| `backfill.max_initial_messages` | `10000` | Max messages to backfill per chat |
| `encryption.allow` | `true` | Enable end-to-bridge encryption |
| `database.type` | `sqlite3-fk-wal` | `sqlite3-fk-wal` or `postgres` |

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
  ├── connector.go           #   bridge lifecycle + macOS permissions
  ├── client.go              #   send/receive/reactions/edits/typing
  ├── login.go               #   Apple ID + 2FA login flow
  ├── chatdb.go              #   chat.db backfill + contacts (macOS)
  ├── ids.go                 #   identifier/portal ID conversion
  ├── capabilities.go        #   supported features
  └── config.go              #   bridge config schema
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
rustpush/                    # OpenBubbles/rustpush (vendored)
nac-validation/              # Local NAC via AppleAccount.framework
imessage/                    # macOS chat.db + Contacts reader
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
