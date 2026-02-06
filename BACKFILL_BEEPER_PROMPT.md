# Backfill Not Working via bbctl/Beeper — Reproduce and Fix

## Problem

A user running the bridge via Beeper's bridge-manager (`bbctl`) on an M1 Mac Mini reports that backfill doesn't work. He confirmed:
- Terminal.app has Full Disk Access granted
- Running the bridge from Terminal.app directly (not via LaunchAgent)
- Backfill still doesn't work

On the standalone setup (LaunchAgent, `make install`), backfill works fine.

## Environment

- **Mac VM**: `tart` VM named `imessage-bridge-test` at `192.168.65.2`
  - SSH in and set up the Beeper bbctl flow to reproduce the issue
  - If SSH creds don't work, check with the user or use `tart run imessage-bridge-test` for GUI access
- **Bridge source**: `~/colter/repos/metamatrix/mautrix-imessage/`
- **Host machine**: Apple Silicon, macOS 26.1

## Setup Steps (on the VM)

1. Install prerequisites: `brew install go rust libolm protobuf`
2. Install bbctl: download from https://github.com/beeper/bridge-manager/releases (arm64 macOS)
3. `bbctl login` (will need Beeper account — may need to create a test one or use existing)
4. `bbctl config --type bridgev2 sh-imessage` — generates config at `~/.local/share/bbctl/sh-imessage/config.yaml`
5. Clone and build: `git clone https://github.com/lrhodin/imessage.git && cd imessage && make build`
6. Run: `./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c ~/.local/share/bbctl/sh-imessage/config.yaml`
7. Login via bot, then check if backfill works

## Debugging the Backfill Issue

The backfill code path is:

1. `Connect()` in `pkg/connector/client.go` calls `openChatDB(log)` 
2. `openChatDB()` in `pkg/connector/chatdb.go` calls `canReadChatDB()` from `permissions_darwin.go`
3. `canReadChatDB()` tries to open `~/Library/Messages/chat.db` and run a test query
4. If it succeeds, `chatDB` is set and `syncChatsFromDB()` runs

### Likely causes (investigate in order):

1. **Working directory matters**: The bridge creates a `state/` directory relative to CWD for keystore, anisette, etc. When run via bbctl, the CWD might be different. Check if `chat.db` path resolution uses `$HOME` correctly regardless of CWD.

2. **`canReadChatDB()` silently returns false**: The function returns a bool with no logging. Add debug logging to see exactly what's failing — is `sql.Open` failing? Is the test query failing? What's the error?

3. **The `imessage/mac` blank import might not work**: `chatdb.go` has `_ "github.com/lrhodin/imessage/imessage/mac"` to register the mac platform. Check if `imessage.NewAPI()` is actually finding the registered platform.

4. **`chat.db` might not exist in the VM**: The VM might not have Messages set up. Ensure iMessage is signed in and has some message history.

5. **TCC/FDA in the VM**: Even with Terminal.app having FDA, the VM environment might handle TCC differently. Check `log show --predicate 'subsystem == "com.apple.TCC"' --last 5m` after a failed attempt.

### Key files:
- `pkg/connector/chatdb.go` — `openChatDB()`, `FetchMessages()`
- `pkg/connector/permissions_darwin.go` — `canReadChatDB()`, `showDialogAndOpenFDA()`
- `pkg/connector/client.go` — `Connect()` (calls `openChatDB`), `syncChatsFromDB()`

## Fix

1. Add verbose logging to `canReadChatDB()` and `openChatDB()` so we can see exactly what fails
2. Fix whatever the root cause is
3. Test both the bbctl flow and the standalone `make install` flow to ensure both work
4. `make build` must succeed after changes
