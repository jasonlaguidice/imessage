# mautrix-imessage

A Matrix-iMessage puppeting bridge. Bridges iMessage conversations into Matrix rooms so you can read and reply to iMessages from any Matrix client.

Two connectors are included:

| | **mac** (chat.db) | **rustpush** (direct protocol) |
|---|---|---|
| How it works | Reads `~/Library/Messages/chat.db`, sends via AppleScript | Connects directly to Apple's iMessage servers via rustpush |
| Requires | Full Disk Access | Apple ID login (via bridge bot) |
| Send messages | ✅ | ✅ |
| Receive messages | ✅ (polling, ~5s delay) | ✅ (real-time, instant) |
| Reactions / tapbacks | Receive only | ✅ Send + receive |
| Edits / unsends | ❌ | ✅ Send + receive |
| Typing indicators | ❌ | ✅ Send + receive |
| Read receipts | Receive only | ✅ Send + receive |
| Attachments | ✅ | ✅ (MMCS) |
| Message history backfill | ✅ | ❌ |
| Contact name resolution | ✅ | ❌ |

Both connectors can run simultaneously — rustpush handles real-time messaging while the mac connector provides backfill and contact resolution.

## Quick Start

### Prerequisites

- **macOS 14+** (Apple Silicon or Intel)
- **Go 1.24+** — `brew install go`
- **Rust** (stable) — `brew install rust` or [rustup.rs](https://rustup.rs)
- **libolm** — `brew install libolm`
- **protobuf** — `brew install protobuf`
- A running **Matrix homeserver** (e.g. [Synapse](https://element-hq.github.io/synapse/))

### 1. Build

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make build
```



### 2. Configure

Generate an example config, then edit it:

```bash
mkdir data
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c data/config.yaml -e
```

Open `data/config.yaml` and set these values:

```yaml
# ── These are the only fields you need to change ──

homeserver:
    address: http://localhost:8008       # URL where the bridge can reach your homeserver
    domain: yourserver.tld               # The server_name from your homeserver config

bridge:
    permissions:
        "@you:yourserver.tld": admin     # Your Matrix ID

network:
    platform: mac                        # "mac" or "rustpush-local" (see below)
```

The defaults work for everything else (SQLite database, port 29332, etc.). See the comments in the generated file for all available options.

#### Choosing a platform

- **`mac`** — Reads iMessages from the local chat.db database. Requires Full Disk Access for the bridge process. Good for: backfill, contact names, simple setup.
- **`rustpush-local`** — Connects directly to Apple's iMessage service. Requires Apple ID login through the bridge bot. Good for: real-time delivery, reactions, edits, typing indicators.
- **Both** — Set `platform: mac`, then also log in with Apple ID via the bridge bot. The bridge runs both connectors: rustpush for real-time, mac for backfill. This is the recommended setup.

### 3. Register with your homeserver

Generate the appservice registration file:

```bash
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c data/config.yaml -g
```

Then register it with your homeserver. For Synapse, add to `homeserver.yaml`:

```yaml
app_service_config_files:
  - /path/to/data/registration.yaml
```

Restart Synapse after adding the registration.

### 4. Start the bridge

**Option A — Setup wizard** (recommended for mac connector):

```bash
make install
```

This opens a wizard that checks Full Disk Access, requests Contacts permission, and installs a LaunchAgent so the bridge starts at login and auto-restarts on crash.

**Option B — Run directly:**

```bash
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c data/config.yaml
```

**Option C — LaunchAgent manually:**

```bash
# Install LaunchAgent (starts at login, auto-restarts)
make install

# Or manage it directly
launchctl load ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist
launchctl unload ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist
```

### 5. Login

DM the bridge bot (`@imessagebot:yourserver.tld`) in your Matrix client.

**For the mac connector:**

Send `login` and select **Verify**. The bridge confirms it can read your iMessage database.

**For rustpush:**

Send `login` and select **Apple ID (rustpush)**. Enter your Apple ID email, password, and 2FA code when prompted. The bridge registers with Apple's iMessage servers and starts receiving messages in real-time.

You can do both logins — they run simultaneously.

### 6. Start chatting

To message someone, DM the bridge bot:

```
resolve +15551234567
```

or

```
resolve user@icloud.com
```

This creates a portal room bridged to that iMessage conversation. Messages you send in the portal room are delivered as iMessages, and their replies appear in the room.

## Architecture

```
Matrix (Element/etc) ←→ Synapse
                           ↓ appservice
                     mautrix-imessage
                           │
              ┌────────────┴────────────┐
              │                         │
    mac connector                rustpush connector
    (chat.db + AppleScript)      (Apple IDS/APNs)
                                        │
                                 local NAC validation
                                 (AppleAccount.framework)
```

## Configuration Reference

The full config is generated by the `-e` flag. Key sections beyond the basics:

| Section | What it controls |
|---------|-----------------|
| `database.type` | `sqlite3-fk-wal` (default) or `postgres` |
| `database.uri` | Database path/connection string |
| `appservice.address` | Port the bridge listens on (default: `http://localhost:29332`) |
| `bridge.permissions` | Who can use the bridge (`user`, `admin`) |
| `encryption` | End-to-bridge encryption settings |
| `network.platform` | `mac` or `rustpush-local` |

## Management

```bash
# Logs
tail -f data/bridge.stdout.log

# Restart (auto-restarts via LaunchAgent)
launchctl stop com.lrhodin.mautrix-imessage

# Stop completely
launchctl unload ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist

# Remove LaunchAgent
make uninstall
```

## Development

```bash
make build      # Build .app bundle
make rust       # Build Rust library only
make bindings   # Regenerate Go FFI bindings (needs uniffi-bindgen-go)
make clean      # Remove build artifacts
```

After modifying Rust code, you may need to clear Go's build cache:

```bash
cargo clean -p rustpushgo   # in pkg/rustpushgo/
go run -a ./cmd/mautrix-imessage/ -c data/config.yaml   # force rebuild
```

### Source layout

```
cmd/mautrix-imessage/        # Entrypoint + macOS setup wizard
pkg/connector/               # bridgev2 connector
  ├── rustpush_client.go     #   rustpush send/receive/reactions/edits
  ├── rustpush_login.go      #   Apple ID + 2FA login flow
  ├── rustpush_capabilities.go
  ├── client.go              #   mac connector client
  ├── handleimessage.go      #   incoming message routing
  ├── connector.go           #   platform routing + dual-connector dedup
  └── ...
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
  └── src/
      ├── lib.rs             #   uniffi exports (messages, login, client)
      ├── local_config.rs    #   LocalMacOSConfig (hardware info, NAC)
      └── hardware_info.m    #   IOKit hardware reader (ObjC)
nac-validation/              # Local NAC via AAAbsintheContext
rustpush/                    # OpenBubbles/rustpush (vendored)
imessage/                    # macOS chat.db + AppleScript + Contacts
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
