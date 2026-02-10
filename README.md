# mautrix-imessage (v2)

A Matrix-iMessage puppeting bridge. Send and receive iMessages from any Matrix client.

This is the **v2** rewrite using [rustpush](https://github.com/OpenBubbles/rustpush) and [bridgev2](https://docs.mau.fi/bridges/general/bridgev2.html) — it connects directly to Apple's iMessage servers without SIP bypass, Barcelona, or relay servers.

**Features**: text, images, video, audio, files, reactions/tapbacks, edits, unsends, typing indicators, read receipts, group chats, SMS forwarding, and contact name resolution.

**Platforms**: macOS (full features) and Linux (via hardware key extracted from a Mac once).

## Quick Start (macOS)

macOS 13+ required (Ventura or later). Sign into iCloud on the Mac running the bridge (Settings → Apple ID) — this lets Apple recognize the device so login works without 2FA prompts.

### With Beeper

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install-beeper
```

The installer handles everything: Homebrew, dependencies, building, Beeper login, iMessage login, config, and LaunchAgent setup.

### With a Self-Hosted Homeserver

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install
```

The installer auto-installs Homebrew and dependencies if needed, asks three questions (homeserver URL, domain, your Matrix ID), generates config files, and starts the bridge as a LaunchAgent. It will pause and tell you exactly what to add to your `homeserver.yaml` to register the bridge.

Once running, DM `@imessagebot:yourdomain` in your Matrix client and send `login`.

## Quick Start (Linux)

The bridge runs on Linux using a hardware key extracted once from a real Mac. No Mac needed at runtime for Intel keys; **Apple Silicon Macs** require the NAC relay (a small background process on the Mac).

### Prerequisites

Ubuntu 22.04+ (or equivalent). Only `git`, `make`, and `sudo` are needed — the build installs everything else:

```bash
sudo apt install -y git make
```

### Step 1: Extract hardware key (one-time, on your Mac)

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
go run tools/extract-key/main.go
```

This reads hardware identifiers (serial, MLB, ROM, etc.) and outputs a base64 key. The Mac is not modified.

**Apple Silicon Macs** lack the encrypted IOKit properties needed by the x86_64 NAC emulator. You must also run the NAC relay:

```bash
# Terminal 1: start the relay (keeps running)
go run tools/nac-relay/main.go

# Terminal 2: extract key with relay URL
go run tools/extract-key/main.go -relay http://<your-mac-ip>:5001/validation-data
```

The relay URL is embedded in the key. Set up port forwarding if the bridge runs outside your LAN (see [NAC Relay](#nac-relay) below).

### Step 2: Build and install the bridge (on Linux)

#### With Beeper

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install-beeper
```

#### With a Self-Hosted Homeserver

```bash
git clone https://github.com/lrhodin/imessage.git
cd imessage
make install
```

On first run expect ~3 minutes for the Rust library to compile.

### Step 3: Login

DM the bridge bot and choose the **"Apple ID (External Key)"** login flow:

1. Paste your hardware key (base64)
2. Enter your Apple ID and password
3. Enter the 2FA code sent to your trusted devices

The bridge registers with Apple's servers and starts receiving iMessages.

### NAC Relay

The NAC relay is required for **Apple Silicon** Macs. It's a tiny HTTP server that generates Apple validation data on demand using the Mac's native `AAAbsintheContext` framework.

**Install as a persistent service (recommended):**

```bash
# Build and install as a LaunchAgent with .app bundle for TCC permissions
go build -o ~/bin/nac-relay ./tools/nac-relay/
~/bin/nac-relay --setup
```

The `--setup` command creates an `.app` bundle at `~/Applications/nac-relay.app`, installs a LaunchAgent, and starts the service. On first run it will prompt for **Full Disk Access** (needed for chat.db backfill) and **Contacts** access — grant both when the system dialogs appear.

```bash
# Check status
tail -f /tmp/nac-relay.log
```

**Port forwarding**: If your bridge runs outside the LAN (e.g., cloud VM), forward port 5001 TCP to your Mac's local IP. Lock the allowed source IPs to your bridge server for security.

**Intel Macs**: The NAC relay is not needed. The bridge runs the x86_64 NAC emulator locally on Linux using hardware data from the extracted key.

### Linux Features with NAC Relay

When the NAC relay is running on a Mac with Full Disk Access and Contacts access granted:

- **Chat history backfill** — messages from the Mac's `chat.db` are synced via the relay, including images and attachments
- **Contact name resolution** — contacts are resolved from the Mac's Contacts.app, so bridged users appear by name instead of phone number / email
- **SMS forwarding** — works the same as macOS (enable Text Message Forwarding on your iPhone)

Without the relay (Intel keys only), chat history starts from when you log in and contacts appear by phone number / email.

## Login

Follow the prompts: Apple ID → password → 2FA (if needed) → handle selection. If the Mac is signed into iCloud with the same Apple ID, login completes without 2FA.

If your Apple ID has multiple identities registered (e.g. a phone number and an email address), you'll be asked which one to use for outgoing messages. This is what recipients see your messages "from". To change it later, set `preferred_handle` in the config (see [Configuration](#configuration)) or log in again.

> **Tip:** In a DM with the bot, commands don't need a prefix. In a regular room, use `!im login`, `!im help`, etc.

### SMS Forwarding

To bridge SMS (green bubble) messages, enable forwarding on your iPhone:

**Settings → Messages → Text Message Forwarding** → toggle on the bridge device.

### Chatting

Incoming iMessages automatically create Matrix rooms. If Full Disk Access is granted (macOS), existing conversations from Messages.app are also synced.

To start a **new** conversation:

```
resolve +15551234567
```

This creates a portal room. Messages you send there are delivered as iMessages.

## How It Works

The bridge connects directly to Apple's iMessage servers using [rustpush](https://github.com/OpenBubbles/rustpush) with local NAC validation (no SIP bypass, no relay server). On macOS with Full Disk Access, it also reads `chat.db` for message history backfill and contact name resolution.

On Linux, NAC validation uses one of two paths:

- **Intel key**: [open-absinthe](rustpush/open-absinthe/) emulates Apple's `IMDAppleServices` x86_64 binary via unicorn-engine, hooking IOKit/CoreFoundation calls and feeding them hardware data from the extracted key
- **Apple Silicon key + relay**: The bridge fetches validation data from a NAC relay running on the Mac, which calls Apple's native `AAAbsintheContext` framework

```mermaid
flowchart TB
    subgraph macos["🖥 macOS"]
        HS1[Homeserver] -- appservice --> Bridge1[mautrix-imessage]
        Bridge1 -- FFI --> RP1[rustpush]
        RP1 -- IOKit/AAAbsinthe --> NAC1[Local NAC]
    end
    subgraph linux["🐧 Linux"]
        HS2[Homeserver] -- appservice --> Bridge2[mautrix-imessage]
        Bridge2 -- FFI --> RP2[rustpush]
        RP2 -- unicorn-engine --> NAC2[open-absinthe]
        RP2 -. "Apple Silicon key" .-> Relay[NAC Relay on Mac]
    end
    Client1[Matrix client] <--> HS1
    Client2[Matrix client] <--> HS2
    RP1 <--> Apple[Apple IDS / APNs]
    RP2 <--> Apple

    style macos fill:#f0f4ff,stroke:#4a6fa5,stroke-width:2px,color:#1a1a2e
    style linux fill:#f0fff4,stroke:#4aa56f,stroke-width:2px,color:#1a1a2e
    style Apple fill:#1a1a2e,stroke:#1a1a2e,color:#fff
    style Client1 fill:#fff,stroke:#999,color:#333
    style Client2 fill:#fff,stroke:#999,color:#333
    style Relay fill:#ffe0b2,stroke:#e65100,color:#333
```

### Real-time and backfill

**Real-time messages** flow through Apple's push notification service (APNs) via rustpush and appear in Matrix immediately.

**Backfill** runs once on first login: the bridge reads `chat.db` and creates portals for all chats with activity in the last `initial_sync_days` (default: 1 year, configurable). On macOS this reads the local database directly; on Linux with the NAC relay, it proxies queries over HTTP to the Mac. After initial sync, everything is real-time via rustpush.

## Management

### macOS

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

### Linux

```bash
# If using systemd (from make install / make install-beeper)
systemctl --user status mautrix-imessage
journalctl --user -u mautrix-imessage -f
systemctl --user restart mautrix-imessage

# If running directly
./mautrix-imessage-v2 -c data/config.yaml
```

### NAC Relay (macOS)

```bash
# View logs
tail -f /tmp/nac-relay.log

# Restart
launchctl kickstart -k gui/$(id -u)/com.imessage.nac-relay

# Stop
launchctl bootout gui/$(id -u)/com.imessage.nac-relay
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
| `network.preferred_handle` | *(from login)* | Outgoing identity (`tel:+15551234567` or `mailto:user@example.com`) |
| `backfill.max_initial_messages` | `10000` | Max messages to backfill per chat |
| `encryption.allow` | `true` | Enable end-to-bridge encryption |
| `database.type` | `sqlite3-fk-wal` | `sqlite3-fk-wal` or `postgres` |

## Development

```bash
make build      # Build .app bundle (macOS) or binary (Linux)
make rust       # Build Rust library only
make bindings   # Regenerate Go FFI bindings (needs uniffi-bindgen-go)
make clean      # Remove build artifacts
```

### Source layout

```
cmd/mautrix-imessage/        # Entrypoint
pkg/connector/               # bridgev2 connector
  ├── connector.go           #   bridge lifecycle + platform detection
  ├── client.go              #   send/receive/reactions/edits/typing
  ├── login.go               #   Apple ID + external key login flows
  ├── chatdb.go              #   chat.db backfill + contacts (macOS)
  ├── ids.go                 #   identifier/portal ID conversion
  ├── capabilities.go        #   supported features
  └── config.go              #   bridge config schema
pkg/rustpushgo/              # Rust FFI wrapper (uniffi)
rustpush/                    # OpenBubbles/rustpush (vendored)
  └── open-absinthe/         #   NAC emulator (unicorn-engine, cross-platform)
nac-validation/              # Local NAC via AppleAccount.framework (macOS)
tools/
  ├── extract-key/           # Hardware key extraction (run on Mac)
  └── nac-relay/             # NAC validation + contacts + backfill relay (run on Mac)
imessage/                    # macOS chat.db + Contacts reader
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
