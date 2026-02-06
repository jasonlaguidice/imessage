# Backfill End-to-End Test on Mac VM with Beeper

## Goal

Reproduce and fix the backfill bug: a user reports backfill doesn't work when running the bridge via Beeper's `bbctl`. Debug logging has been added to `canReadChatDB()` and `openChatDB()`. Deploy to the Mac VM, run via bbctl, and read the logs to find the root cause.

## VM Access

- **VM**: tart VM `imessage-bridge-test` (already running)
- **IP**: Run `tart ip imessage-bridge-test` on the host to get current IP (likely `192.168.65.2`)
- **SSH**: `ssh <user>@<ip>` (credentials at bottom of this file)
- **Host bridge source**: `~/colter/repos/metamatrix/mautrix-imessage/`

## Step 1: Build on host and copy to VM

Build on the host (Apple Silicon, faster):
```bash
cd ~/colter/repos/metamatrix/mautrix-imessage
make build
```

Then copy the built app bundle and necessary files to the VM:
```bash
VM_IP=$(tart ip imessage-bridge-test)
scp -r mautrix-imessage.app $USER@$VM_IP:~/
```

## Step 2: Set up bbctl on the VM

SSH into the VM and:

1. Install bbctl if not already present:
   ```bash
   # Download latest arm64 macOS release from https://github.com/beeper/bridge-manager/releases
   curl -L -o bbctl https://github.com/beeper/bridge-manager/releases/latest/download/bbctl-darwin-arm64
   chmod +x bbctl
   sudo mv bbctl /usr/local/bin/
   ```

2. Login to Beeper (will need a Beeper account — check if one is already configured):
   ```bash
   bbctl login
   ```

3. Generate bridge config:
   ```bash
   bbctl config --type bridgev2 sh-imessage
   ```
   This creates `~/.local/share/bbctl/sh-imessage/config.yaml`

4. Optionally add the network section (should work without it, but just in case):
   ```bash
   cat >> ~/.local/share/bbctl/sh-imessage/config.yaml << 'EOF'
   network:
       displayname_template: "{{if .FirstName}}{{.FirstName}}{{if .LastName}} {{.LastName}}{{end}}{{else}}{{.ID}}{{end}}"
   EOF
   ```

## Step 3: Ensure chat.db exists and FDA is granted

On the VM:
1. Check if Messages has been set up: `ls ~/Library/Messages/chat.db`
2. If not, open Messages.app and sign in to iMessage (or at minimum ensure the file exists)
3. Test read access: `sqlite3 ~/Library/Messages/chat.db "SELECT COUNT(*) FROM message"`
4. If permission denied, grant Full Disk Access to Terminal.app in System Settings → Privacy & Security → Full Disk Access

## Step 4: Run the bridge and capture logs

```bash
cd ~
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c ~/.local/share/bbctl/sh-imessage/config.yaml 2>&1 | tee bridge.log
```

Or run in background:
```bash
./mautrix-imessage.app/Contents/MacOS/mautrix-imessage -c ~/.local/share/bbctl/sh-imessage/config.yaml > bridge.log 2>&1 &
```

## Step 5: Login and test

1. From Beeper, DM `@sh-imessagebot:beeper.local` and send `login`
2. Follow the Apple ID + 2FA flow
3. Once connected, check the logs for backfill-related output:
   ```bash
   grep -i 'chat.db\|backfill\|full disk\|FDA\|chatdb\|openChatDB\|canReadChatDB\|sync' bridge.log
   ```

## What to look for in logs

The debug logging should now show:
- Whether `canReadChatDB()` succeeded or failed, and the exact error if it failed
- Whether `openChatDB()` returned nil and why
- Whether `syncChatsFromDB()` was called and how many chats it found
- Any TCC/permission errors

## Common failure modes

1. **`chat.db` doesn't exist**: Messages never set up in the VM
2. **TCC denies access**: Terminal.app or the binary doesn't have FDA — look for "operation not permitted" in logs
3. **`imessage.NewAPI()` fails**: The mac platform might not register correctly — look for errors from `newBridgeAdapter` or `imessage.NewAPI`
4. **Working directory issue**: The bridge creates `state/` relative to CWD. If CWD is wrong, other paths might also resolve incorrectly. Check what CWD is in the logs.
5. **`chat.db` is empty**: The VM has Messages set up but no message history — backfill would "work" but produce zero messages

## Fix

Once root cause is identified from the debug logs:
1. Fix the issue in the source on the host (`~/colter/repos/metamatrix/mautrix-imessage/`)
2. Rebuild: `make build`
3. Re-copy to VM and re-test
4. Confirm backfill works: portal rooms should get historical messages populated
5. Also verify the standalone flow (`make install`) still works — don't break that

## Credentials

(appended below)
