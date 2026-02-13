# Research Task: iMessage Group Chat Identity Model

## Objective

Produce a comprehensive report (`docs/group-id-research.md`) mapping every identifier involved in iMessage group chat identity — across CloudKit sync, real-time APNs delivery, and the local Messages database. The goal is to understand how Apple tracks "the same conversation" across member changes, and architect the correct portal ID strategy for our Matrix-iMessage bridge.

## The Problem We're Solving

When iMessage group chat membership changes (someone added/removed), Apple appears to assign a new group UUID. Our bridge creates separate Matrix rooms for each UUID, but the user sees ONE conversation thread on their iPhone. We need to understand exactly which identifiers stay stable vs change, and which one(s) to use as the canonical "conversation identity."

## What to Research

### 1. CloudKit Chat Records (`chatEncryptedv2` zone)

Our Rust code decodes these in `rustpush/src/imessage/cloud_messages.rs` (struct `CloudChat`, ~line 231). Examine every field:

- `cid` → `chat_identifier` — What format? (e.g., `chat368136512547052395`, `iMessage;+;chat...`, etc.) Does it stay stable across member changes?
- `gid` → `group_id` — UUID format. When exactly does it change? Is it per-member-change or something else?
- `ogid` → `original_group_id` — Points to what? The immediately previous `gid`? The very first `gid`? Something else?
- `stl` → `style` — We know 43=group, 45=DM. Any other values?
- `ptcpts` → `participants` — How are participants encoded? URIs like `tel:+1...` or `mailto:...`?
- `guid` — What is this? Same as `gid`? Different?
- `name` / `display_name` — User-set group name vs auto-generated?
- `svc` → `service_name` — Always "iMessage"? Can be "SMS"?
- Any other fields that might relate to conversation identity

**Key question**: Given 30+ CloudKit chat records that all represent the same group conversation (with different member snapshots), which field(s) are stable across all of them?

### 2. Real-Time APNs Messages (rustpush)

When a message arrives via APNs push, examine what identifiers are available:

- Look at `rustpush/src/imessage/messages.rs` for incoming message structures
- Look at `pkg/rustpushgo/src/lib.rs` for `WrappedMessage` (around line 340) — what is `sender_guid`?
- Is `sender_guid` the same as CloudKit's `gid`? Or something else?
- For group messages: what fields identify which conversation the message belongs to?
- Is there a `chat_identifier` equivalent in real-time messages?
- When group membership changes, does the real-time `sender_guid` change immediately?

### 3. Local Messages Database (chat.db on macOS)

While our bridge doesn't use chat.db directly (we use CloudKit), understanding Apple's local model helps:

- `chat` table: `ROWID`, `chat_identifier`, `group_id`, `display_name` — which stays stable?
- `chat_message_join` table: how messages link to chats
- When members change, does the `chat.ROWID` stay the same? Does `chat_identifier`?
- Is there a concept of chat "continuation" in the local DB?

### 4. The `original_group_id` Chain

This is the most critical piece to understand:

- When Apple creates a new `gid` (member change), does `ogid` on the new record point to the old `gid`?
- Is it always a direct parent link (A → B → C), or can it skip generations?
- Can `ogid` be empty on the very first version of a group?
- Can multiple records share the same `ogid`? (e.g., two member changes from the same base)
- Is the chain always linear, or can it branch/merge?

### 5. Our Current Data

Query the live database on the bridge to examine real data:

```bash
# SSH to the bridge
gcloud compute ssh imessage-bridge-32 --zone=us-west1-b

# Database location (CWD-relative, the actual DB is here):
sqlite3 ~/imessage/mautrix-imessage.db

# Cloud chat table schema
.schema cloud_chat

# Example: find all cloud_chat records for groups involving specific participants
SELECT cloud_chat_id, group_id, portal_id, display_name, participants_json
FROM cloud_chat WHERE portal_id LIKE 'gid:%' ORDER BY group_id;

# Check for the real-time message that created a duplicate portal
SELECT id, name, mxid FROM portal WHERE id LIKE 'gid:2f787cd8%';
```

Also look at the Rust source to understand what CloudKit fields are available but not yet stored:
- `rustpush/src/imessage/cloud_messages.rs` — `CloudChat` struct
- `pkg/rustpushgo/src/lib.rs` — `WrappedCloudSyncChat` FFI struct  
- `pkg/connector/cloud_backfill_store.go` — DB schema and upsert logic
- `pkg/connector/sync_controller.go` — `resolvePortalIDForCloudChat()` — current portal ID resolution logic
- `pkg/connector/client.go` — `makePortalKey()` (~line 2174) — real-time portal ID resolution

## Expected Output

Produce `docs/group-id-research.md` containing:

1. **Identifier Map**: A table listing every ID field, its source (CloudKit/APNs/chatdb), format, stability characteristics, and whether it changes on member changes
2. **Chain Analysis**: Document the `original_group_id` chain behavior with examples from real data
3. **Stability Analysis**: Which identifier(s) remain constant across the lifetime of a single conversation?
4. **Architecture Recommendation**: Based on findings, what should we use as the canonical portal ID for group chats? How should we handle:
   - Initial cloud sync (bootstrap)
   - Incremental cloud sync (new chat records arriving)
   - Real-time messages (APNs push with possibly-new UUID)
   - The transition period (real-time message arrives before CloudKit syncs the new chat record)
5. **Data Model Changes**: What columns/tables/indexes need to change in our bridge DB?

## Important Context

- The bridge is a Go application with a Rust FFI layer for Apple protocol handling
- We're bridging iMessage to Matrix via the mautrix framework
- Each Matrix room = one "portal" identified by a portal ID string
- Currently using `gid:<lowercase-uuid>` for groups, `tel:+1234567890` or `mailto:user@example.com` for DMs
- The duplicate portal problem: real-time message for "Ludvig, David, & James" created `gid:2f787cd8-5e31-4ed6-802c-4e1b7ee56eff` but CloudKit has ~30 different UUIDs for what appears to be the same conversation (with varying membership over time)
- We do NOT have access to the local macOS chat.db — the bridge runs on a Linux VM with only CloudKit + APNs access
