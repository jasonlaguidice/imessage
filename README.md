# mautrix-imessage

A Matrix-iMessage puppeting bridge built on the [mautrix bridgev2](https://docs.mau.fi/bridges/) megabridge framework. Reads from the native macOS iMessage database (`~/Library/Messages/chat.db`) and sends via AppleScript.

Forked from [mautrix/imessage](https://github.com/mautrix/imessage) and ported from the legacy bridge framework to bridgev2.

## Quick Start

### Prerequisites

- **macOS 13+** (Apple Silicon or Intel)
- **Go** — `brew install go`
- **libolm** — `brew install libolm`
- A running Matrix homeserver (e.g. [Synapse](https://element-hq.github.io/synapse/))

### Build & Install

```bash
make build     # build .app bundle
make install   # build + run setup wizard
```

`make install` opens a setup wizard that:

1. Checks **Full Disk Access** — if missing, opens System Settings and waits for you to grant it
2. Requests **Contacts** access — shows the native macOS permission prompt
3. Installs a **LaunchAgent** so the bridge starts at login and auto-restarts on crash

After setup, the bridge is running. No scripts, no manual steps.

### First Login

DM `@imessagebot:matrix.local` (or whatever your bridge bot is named), send `login`, and select **verify**. The bridge confirms it can read your iMessage database and starts bridging.

### Management

```bash
# The bridge runs automatically via LaunchAgent
launchctl stop com.lrhodin.mautrix-imessage       # restart (auto-restarts)
launchctl unload ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist  # fully stop
launchctl load ~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist    # start

make uninstall  # remove LaunchAgent
make clean      # remove .app bundle
```

Logs: `mautrix-imessage-data/bridge.stdout.log`

## Configuration

Before running `make install`, set up `mautrix-imessage-data/config.yaml`:

```bash
# Generate example config
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c mautrix-imessage-data/config.yaml -e

# Generate registration for your homeserver
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c mautrix-imessage-data/config.yaml -g
```

Key config fields:

| Field | Value |
|-------|-------|
| `homeserver.address` | Your Synapse URL (e.g. `http://localhost:8008`) |
| `homeserver.domain` | Your server name (e.g. `matrix.local`) |
| `database.uri` | Postgres connection string |
| `bridge.permissions` | Map of Matrix IDs to permission levels |

Copy the generated `registration.yaml` to your homeserver's appservice config directory. If Synapse runs in Docker, change the `url` field to `http://host.docker.internal:29332`.

## Capabilities

| Feature | Receive | Send |
|---------|---------|------|
| Text messages | ✅ | ✅ |
| Attachments (images, files) | ✅ | ✅ |
| Tapbacks / reactions | ✅ | ❌ |
| Read receipts | ✅ | ❌ |
| Group name changes | ✅ | ❌ |
| Typing notifications | ❌ | ❌ |
| Message history backfill | ✅ | — |
| Contact name resolution | ✅ | — |

Sending limitations are inherent to the macOS AppleScript connector.

## Architecture

```
Matrix homeserver ◄──appservice──► mautrix-imessage.app
                                       │
                    ┌──────────────────┤
                    ▼                  ▼
          ~/Library/Messages/    AppleScript
             chat.db (read)      Messages.app (send)
```

The bridge runs as a native macOS `.app` bundle (`LSUIElement` — no Dock icon) with its own TCC identity for Full Disk Access and Contacts permissions. A LaunchAgent keeps it running across reboots.

## Development

```bash
make build   # build .app bundle to ../mautrix-imessage.app
make clean   # remove .app bundle
```

The Makefile handles CGO flags for Homebrew's libolm, code signing (ad-hoc), and Info.plist bundling.

Source layout:

```
cmd/mautrix-imessage/    # entrypoint + setup wizard
pkg/connector/           # bridgev2 connector (13 files)
imessage/mac/            # macOS SQLite + AppleScript + Contacts (CGO/Obj-C)
imessage/interface.go    # platform-agnostic iMessage API interface
imessage/struct.go       # iMessage data types
imessage/tapback.go      # tapback/reaction parsing
ipc/                     # IPC utilities (legacy, minimal use)
Info.plist               # macOS app bundle metadata
Makefile                 # build system
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
