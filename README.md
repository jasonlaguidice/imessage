# Rustpush-Matrix

A Matrix-iMessage puppeting bridge. Built using [rustpush](https://github.com/OpenBubbles/rustpush) (direct Apple IDS/APNs connection) and the [mautrix-go bridgev2](https://github.com/mautrix/go) framework.

**Platforms**: macOS 13+, Linux (via hardware key extracted once from a Mac).

> Contact Key Verification must be disabled on your Apple ID for the bridge to function.

## Features

- Text, images, video, audio, files
- Reactions/tapbacks, edits, unsends
- Typing indicators, read receipts
- Group chats, SMS forwarding
- Contact name resolution (iCloud, external CardDAV)
- CloudKit message history backfill
- FaceTime web-join links (for non-Apple platforms)
- iOS 18 Focus / Do Not Disturb status
- iCloud Shared Albums
- Name & Photo Sharing fallback for unknown senders

## Installation

### Docker (Linux — recommended)

Docker images are available for `linux/amd64`:

```bash
docker pull ghcr.io/jasonlaguidice/imessage:latest
```

See [docker-compose.yml](docker-compose.yml) for a reference deployment.

Before the bridge can connect to Apple, you need a hardware key extracted from a Mac — see [Linux Hardware Key](#linux-hardware-key) below.

### macOS

```bash
git clone https://github.com/jasonlaguidice/imessage.git
cd imessage
make install
```

The installer handles dependencies, config, login, and LaunchAgent setup. Re-run it at any time to change settings (FaceTime, StatusKit, HEIC conversion, video transcoding, CardDAV, etc.) without wiping your data.

### Generic (Linux, self-hosted)

```bash
git clone https://github.com/jasonlaguidice/imessage.git
cd imessage
make install
```

The installer builds the Rust library, walks through config, and registers a systemd user service.

### Beeper

Pass `--type bridgev2` config from `bbctl` and point it at the binary:

```bash
bbctl config --type bridgev2 sh-imessage > config.yaml
./mautrix-imessage-v2 -c config.yaml
```

## Linux Hardware Key

The bridge needs hardware identifiers from a real Mac to authenticate with Apple's IDS layer. This is a one-time extraction.

**Intel Mac** — extract the key once; the Mac is not needed at runtime:

```bash
# GUI app (recommended)
cd tools/extract-key-app && ./build.sh
# Copy ExtractKey.app to the Mac and run it

# Or CLI (macOS 13+, requires Go)
go run tools/extract-key/main.go
```

**Apple Silicon Mac** — requires the NAC relay from this project running on the Mac whenever the bridge is online:

```bash
# GUI menubar app (recommended)
cd tools/nac-relay-app && ./build.sh && open NACRelay.app

# Or CLI
go build -o ~/bin/nac-relay ./tools/nac-relay/
~/bin/nac-relay --setup
```

Then extract the key with the relay URL embedded:

```bash
go run tools/extract-key/main.go -relay https://<mac-ip>:5001/validation-data
```

Paste the base64 key when the login flow asks for it.

## Login

Login runs automatically at the end of `make install`. To log in later (or re-login), DM the bridge bot in the Matrix management room and run the **Apple ID (External Key)** flow.

Prompts: Apple ID → password → 2FA (if needed) → handle selection.

## Usage

In the **management room** (bot DM), commands run bare:

```
start-chat [+15551234567 | user@icloud.com]
contacts
restore-chat
logout
help
```

In **portal rooms** (bridged DMs/groups), prefix with `!im`:

```
!im facetime
!im help
```

## Configuration

Config lives at `~/.local/share/mautrix-imessage/config.yaml`. Key options:

| Field | Default | Description |
|---|---|---|
| `cloudkit_backfill` | `false` | Enable iMessage history backfill from iCloud |
| `disable_facetime` | `false` | Suppress FaceTime commands and notices (set true if you have a native Apple device) |
| `statuskit_notifications` | `true` | Post Focus/DND notices when contacts silence notifications |
| `video_transcoding` | `false` | Auto-remux non-MP4 video to MP4 (requires `ffmpeg`) |
| `heic_conversion` | `false` | Auto-convert HEIC images to JPEG (requires `libheif`) |
| `preferred_handle` | *(from login)* | Outgoing iMessage identity (`tel:+1...` or `mailto:...`) |
| `carddav.*` | *(unset)* | External CardDAV for contact resolution (Google, Nextcloud, Fastmail, etc.) |

## Development

```bash
make build      # Go + Rust
make rust       # Rust library only
make bindings   # Regenerate Go FFI bindings
make clean
```

| Package | Description |
|---|---|
| `pkg/connector/` | Bridge business logic (48 files) |
| `pkg/rustpushgo/` | Rust FFI wrapper (uniffi → cgo) |
| `rustpush/open-absinthe/` | NAC emulator for Linux (Intel key path) |
| `tools/` | Key extraction and NAC relay apps |
| `scripts/` | Install scripts for all four variants |
| `cmd/mautrix-imessage/` | Bridge entrypoint |

## Support

- **Matrix room**: [#matrix-rustpush-bridge:shadowdrake.org](https://matrix.to/#/#matrix-rustpush-bridge:shadowdrake.org)
- **License**: AGPL-3.0 — see [LICENSE](LICENSE)
