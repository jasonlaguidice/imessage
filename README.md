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
- FaceTime call notices (incoming and missed calls; answer on an Apple device)
- iOS 18 Focus / Do Not Disturb status
- iCloud Shared Albums
- Name & Photo Sharing fallback for unknown senders

## Installation

This is a standard [mautrix bridgev2](https://docs.mau.fi/bridges/) bridge for self-hosted Matrix homeservers. Setup follows the usual mautrix flow: generate a config, edit it, generate an appservice registration, register it with your homeserver, run the bridge, then log in through the bridge bot.

On Linux the bridge also needs a hardware key extracted from a Mac before it can connect to Apple — see [Linux Hardware Key](#linux-hardware-key) below.

### 1. Get the bridge

**Docker (Linux — recommended).** Images are available for `linux/amd64`:

```bash
docker pull ghcr.io/jasonlaguidice/imessage:latest
```

See [compose.yml](compose.yml) for a reference deployment. It mounts `./data` at `/data` (`config.yaml`, `registration.yaml`, database) and `./state` at `/state` (Apple session and per-user state), and publishes the appservice port `29332`.

**From source (Linux or macOS).** See [Development](#development) for toolchain requirements.

```bash
git clone https://github.com/jasonlaguidice/imessage.git
cd imessage
make build
```

`make build` runs `make check-deps` first, which installs missing build dependencies (Homebrew on macOS, `scripts/bootstrap-linux.sh` on Linux). It produces `./matrix-rustpush` on Linux and `matrix-rustpush.app` (binary at `matrix-rustpush.app/Contents/MacOS/matrix-rustpush`) on macOS.

### 2. Generate and edit the config

```bash
./matrix-rustpush -c config.yaml -e

# Docker
mkdir -p data state
docker run --rm -v "$PWD/data:/data" ghcr.io/jasonlaguidice/imessage:latest -c /data/config.yaml -e
```

Edit `config.yaml`:

- `homeserver.address` / `homeserver.domain` — your homeserver's URL and server name.
- `appservice.address` — the URL your homeserver uses to reach the bridge (default port `29332`). `appservice.hostname` / `appservice.port` set where the bridge listens; use `0.0.0.0` under Docker.
- `database` — `sqlite3-fk-wal` or `postgres`, plus the URI.
- `bridge.permissions` — who may use (`user`) and administer (`admin`) the bridge.
- `network` — bridge-wide defaults (see [Configuration](#configuration)).

### 3. Register the bridge with your homeserver

```bash
./matrix-rustpush -c config.yaml -g -r registration.yaml

# Docker
docker run --rm -v "$PWD/data:/data" ghcr.io/jasonlaguidice/imessage:latest -c /data/config.yaml -g -r /data/registration.yaml
```

Add `registration.yaml` to your homeserver's appservice config (for Synapse, list it under `app_service_config_files` in `homeserver.yaml`) and restart the homeserver. Re-run `-g` whenever you change the `appservice` section.

### 4. Run it

**Docker:** `docker compose -f compose.yml up -d` (the image runs `matrix-rustpush -c /data/config.yaml`).

**systemd:** a minimal unit:

```ini
[Unit]
Description=matrix-rustpush iMessage bridge
After=network-online.target

[Service]
User=matrix-rustpush
WorkingDirectory=/opt/matrix-rustpush
ExecStart=/opt/matrix-rustpush/matrix-rustpush -c config.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Apple session state is stored in `$XDG_DATA_HOME/mautrix-imessage` (default `~/.local/share/mautrix-imessage`) of the user running the bridge.

On macOS, run `matrix-rustpush.app/Contents/MacOS/matrix-rustpush -c config.yaml`.

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

Start a DM with the bridge bot (`@imessagebot:<your domain>` by default) and send `login`. On Linux, pick the **Apple ID (External Key)** flow; on macOS, **Apple ID** is also offered.

Prompts: Apple ID → password → 2FA (if needed) → handle selection. The flow also asks for your per-user settings: video transcoding, HEIC conversion, CloudKit backfill, contact source (iCloud or external CardDAV), FaceTime call notices and StatusKit notifications.

Some of these can be changed later with bot commands: `set-video-transcoding on|off`, `set-heic-conversion on|off`, `set-carddav` (contact source) and `set-identity` (outgoing handle). Run `help` for the full list.

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
!im help
```

## Configuration

Bridge-specific options live in the `network` section of `config.yaml`. These are the global defaults; the per-user choices made during login override them. Key options:

| Field | Default | Description |
|---|---|---|
| `cloudkit_backfill` | `false` | Enable iMessage history backfill from iCloud |
| `disable_facetime` | `false` | Suppress the incoming / missed FaceTime call notices |
| `statuskit_notifications` | `true` | Post Focus/DND notices when contacts silence notifications |
| `video_transcoding` | `false` | Auto-remux non-MP4 video to MP4 (requires `ffmpeg`) |
| `heic_conversion` | `false` | Auto-convert HEIC images to JPEG (requires `libheif`) |
| `preferred_handle` | *(from login)* | Fallback outgoing iMessage identity (`tel:+1...` or `mailto:...`); a user's own login / `set-identity` choice wins |

External CardDAV contacts (Google, Nextcloud, Fastmail, etc.) have no global setting: each user configures their own server during login or with `set-carddav`.

### Recommended bridge settings

These mautrix keys sit outside the `network` section. The bridge does not change them at runtime; it logs a warning at startup when one is below the recommended value.

- `bridge.unknown_error_auto_reconnect: 5m` (at least `1m`; lets the receive-wedge watchdog rebuild a stuck client)
- `bridge.unknown_error_max_auto_reconnects: 100` (bridgev2 never resets this counter until restart)
- `bridge.phone_numbers_in_profile: true` (keeps `tel:` identifiers in ghost profiles for call buttons and phone contacts)

With `network.cloudkit_backfill: true`:

- `backfill.enabled: true` (required: if it is `false`, the bridge enables it in memory and logs a warning, because CloudKit backfill cannot work without it)
- `backfill.max_initial_messages: 2147483647` (uncapped; any smaller value is treated as a per-chat cap and turns off backward backfill)
- `backfill.max_catchup_messages: 2147483647` (keep equal to `max_initial_messages`)
- `backfill.queue.enabled: true`
- `backfill.queue.batch_size: 10000` (more than 100; small batches fragment room history)

## Development

Building from source needs **Rust 1.95 or newer** (rustpush uses `atomic_try_update`, stable since 1.95). The Makefile checks this before compiling and fails with a clear error on older toolchains. `make check-deps` on Linux runs `rustup update stable` when the installed stable is too old; elsewhere run it yourself, or build with a specific installed toolchain via `RUSTUP_TOOLCHAIN=<version> make build`. The Docker image and CI always install the latest stable.

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
| `scripts/` | Build helpers: `patch_bindings.py` (`make bindings`), `bootstrap-linux.sh` (`make check-deps`) |
| `cmd/matrix-rustpush/` | Bridge entrypoint |

## Support

- **Matrix room**: [#matrix-rustpush-bridge:shadowdrake.org](https://matrix.to/#/#matrix-rustpush-bridge:shadowdrake.org)
- **License**: AGPL-3.0 — see [LICENSE](LICENSE)
