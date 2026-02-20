# TPP: Robust Backfill Implementation

## Status
- **Phase**: Implementation — Phases 1-2 complete, Phase 3 (docs) remaining
- **Last updated**: 2026-02-20 (Session 3)
- **Branch**: `refactor` on `mackid1993/imessage-test`
- **Next step**: Phase 3 (doc updates) or deploy & test Phases 1-2

---

## Tribal Knowledge (READ FIRST)

- **NEVER modify `rustpush/` (vendored)**. All Rust changes go in `pkg/rustpushgo/src/lib.rs`.
- `runCloudSyncController` intentionally has **NO** `defer recover()` — a panic must crash the process. See TPP-backfill-reply-and-panics.md.
- `purgeCloudMessagesByPortalID` **hard-deletes** rows (tombstone path). `deleteLocalChatByPortalID` **soft-deletes** (Beeper/iPhone delete path). This difference is intentional.
- `deleted_chats.json` was deliberately removed. Don't bring it back. The replacement is UUID-based echo detection via `hasMessageUUID`.
- **`pending_cloud_deletion` is a STALE DESIGN.** The docs describe it but we don't delete from CloudKit anymore, so it should never be implemented. Don't build it.
- Failed approaches (don't repeat): deleted_portal DB table, timestamp-based echo detection, blanket is_from_me block, nuking recoverableMessageDeleteZone, `pending_cloud_deletion`. See `docs/deletion-echo-suppression.md`.
- `RECONNECT_WINDOW_MS` is already **30 seconds** (not 10). Docs that say 10s are stale.
- The `isStaleForDeletedChat` compile error and `loginLog` unused variable documented in the deletion-resurrection report have **already been fixed**. The `skipPortals` check in `createPortalsFromCloudSync` replaced `isStaleForDeletedChat`.
- **bridgev2 `doForwardBackfill` calls `FetchMessages` exactly once** — it does NOT loop on `HasMore: true` for forward backfill. Only backward backfill (`DoBackwardsBackfill`) paginates. Do not return `HasMore: true` with `Forward: true`.
- **DM portals can have `cloud_message` rows without any `cloud_chat` entry.** DMs are resolved from message data via `resolveConversationID`. Never delete messages solely based on missing `cloud_chat` — it will destroy DM data.
- **Forward backfill with uncached attachments hangs.** Each `compileBatchMessage` call for an uncached attachment triggers a sequential 90s CloudKit download. For large portals this creates multi-hour stalls. Solution: `preUploadChunkAttachments` parallel-downloads uncached attachments per chunk BEFORE the conversion loop.
- **Failed forward chunking approaches** (don't repeat): returning `HasMore: true` on forward (bridgev2 ignores it, only 5k of 27k delivered); skipping uncached attachments (user rejected — missing data).

---

## Goal

Close the remaining gaps in backfill reliability. Cross-referencing the three design documents against the actual codebase revealed that several documented bugs are already fixed. This TPP addresses the real remaining issues: attachment reliability, large-portal performance, and housekeeping.

---

## Weak-Point Analysis

### Source documents
- `docs/cloudkit-backfill-infrastructure.md`
- `docs/cloudkit-deletion-resurrection-report.md`
- `docs/deletion-echo-suppression.md`

### Already fixed (documented bugs that no longer exist in code)

| Doc Bug | Status |
|---------|--------|
| `isStaleForDeletedChat` compile error | **Fixed.** Replaced with `skipPortals` check. |
| `loginLog` unused variable | **Fixed.** No longer present. |
| `cloudSyncDone` set on sync failure | **Fixed.** Retry loop holds APNs gate closed. |
| `IsStoredMessage` 10s window | **Fixed.** Already 30s. |
| `pending_cloud_deletion` not implemented | **Stale design.** We don't delete from CloudKit anymore. Not needed. |

### Still open

**W1. Failed attachments silently dropped, permanently lost after `fwd_backfill_done=1`** (Medium)

When `safeCloudDownloadAttachment` times out or errors, the attachment is silently skipped. Once `fwd_backfill_done=1` is set (via `CompleteCallback`), `preUploadCloudAttachments` skips the entire portal on restart — including failed attachments. They're permanently lost from the forward backfill path.

**W2. Large-portal first-backfill stall (27k+ messages)** (Medium)

Forward `FetchMessages` returns all messages in one batch (`HasMore: false`). `compileBatchMessage × N` + `BatchSend` for tens of thousands of messages takes 2+ minutes, blocking the portal event loop. The "event handling is taking long" warning fires every 30s. Forward chunking (returning `HasMore: true` with 5k batches) would mitigate this.

**W3. Orphaned `cloud_message` rows after unresolved CloudKit tombstones** (Low)

When a tombstone's `record_name` was never stored locally in `cloud_chat`, no soft-delete of messages occurs. These rows don't cause resurrection (portals need a `cloud_chat` entry), but accumulate as dead storage.

**W4. `cloud_attachment_cache` grows unbounded** (Low)

No cleanup on portal deletion. Not cleared by `clearAllData`. Each entry is small but accumulates indefinitely.

**W5. `preUploadCloudAttachments` ignores context cancellation** (Low)

Called with `context.Background()`. All 32 download goroutines run to completion (or 90s timeout) even if `Disconnect()` is called. Worst case: 48 minutes of leaked work.

**W6. `uploaded.Add(1)` counts attempts, not successes** (Trivial)

Counter increments even when download/upload failed. Misleading log output.

**W7. `donePorals` typo** (Trivial)

Variable named `donePorals` (missing 't'). Cosmetic only.

**W8. Leaked goroutines on CloudKit download stall** (Low — accepted risk)

Bounded to 32 concurrent leaks. No fix without Rust-side cancellation. Document and accept.

**W9. `findAndDeleteCloudChatByIdentifier` CloudKit watermark risk** (Low — accepted risk)

May consume server-side cursor. Accept and document.

---

## Task Breakdown

### Phase 1: Attachment Reliability (Medium)

- [x] **Task 1**: Track failed attachment downloads. Added `failedAttachmentEntry` struct with retry count, `recordAttachmentFailure` helper, `failedAttachments sync.Map` on `IMClient`. Downloads/uploads record failures with attempt count. `preUploadCloudAttachments` retries failed attachments (even for done portals) up to `maxAttachmentRetries=3`, then abandons permanently corrupted records. Fixed `uploaded.Add(1)` to only count successes. Fixed `donePorals` → `donePortals`. Added `failed` count to completion log.
  - Files: `pkg/connector/client.go`

- [x] **Task 2**: Forward backfill internal chunking. Since bridgev2 `doForwardBackfill` calls `FetchMessages` exactly once (no HasMore loop), implemented internal chunking: `FetchMessages` loops over `listOldestMessages` / `listForwardMessages` in 5,000-row chunks, advancing a cursor. Added `preUploadChunkAttachments` — parallel-downloads uncached attachments (up to 32 concurrent) per chunk BEFORE the sequential conversion loop, preventing 90s CloudKit download stalls.
  - Files: `pkg/connector/client.go`, `pkg/connector/cloud_backfill_store.go`

### Phase 2: Cleanup & Housekeeping (Low)

- [x] **Task 3**: `cloud_attachment_cache` pruning via `pruneOrphanedAttachmentCache`. Uses `json_each` + `json_extract` to find record_names referenced by live messages. Also added `cloud_attachment_cache` to `clearAllData`. Called from new `runPostSyncHousekeeping` after bootstrap.
  - Files: `pkg/connector/cloud_backfill_store.go`, `pkg/connector/sync_controller.go`

- [x] **Task 4**: Cancellable context for `preUploadCloudAttachments`. Replaced `context.Background()` with `context.WithCancel` derived from `stopChan` in `runCloudSyncController`. Added `ctx.Done()` select on semaphore acquire so goroutines waiting for slots exit immediately on shutdown.
  - Files: `pkg/connector/sync_controller.go`, `pkg/connector/client.go`

- [x] **Task 5**: Orphaned `cloud_message` cleanup via `deleteOrphanedMessages`. Deletes rows where `deleted=TRUE` AND `portal_id` has no matching `cloud_chat` entry. **Critical restriction**: DM portals legitimately have messages without `cloud_chat` rows, so only already-soft-deleted rows are cleaned up. Called from `runPostSyncHousekeeping`.
  - Files: `pkg/connector/cloud_backfill_store.go`, `pkg/connector/sync_controller.go`

### Phase 3: Documentation (Low)

- [ ] **Task 6**: Update docs. Remove stale bug references (compile errors, 10s window — all fixed). Mark `pending_cloud_deletion` as stale/not-applicable. Document accepted risks (W8, W9). Update `RECONNECT_WINDOW_MS` references from 10s to 30s.
  - Files: `docs/cloudkit-deletion-resurrection-report.md`, `docs/cloudkit-backfill-infrastructure.md`, `docs/deletion-echo-suppression.md`

---

## Required Reading

- `docs/cloudkit-backfill-infrastructure.md` — full architecture reference
- `docs/cloudkit-deletion-resurrection-report.md` — bug catalog (partially stale)
- `docs/deletion-echo-suppression.md` — echo suppression layers (parts are stale)
- `_todo/TPP-backfill-reply-and-panics.md` — prior session context (panic hardening)

---

## Implementation Log

### Session 1 (2026-02-20) — Research only, no code changes
- Read all three reference documents + existing TPP (backfill-reply-and-panics)
- Cross-referenced docs against actual codebase (full exploration of sync_controller.go, client.go, cloud_backfill_store.go, lib.rs)
- **Key finding**: 4 of 7 documented bugs already fixed in code but docs are stale
- **Key finding**: `pending_cloud_deletion` is stale design — we don't delete from CloudKit anymore. Not needed.
- Completed weak-point analysis (9 open items after removing fixed/stale)
- Created task breakdown (6 tasks across 3 phases)

### Session 2 (2026-02-20) — Phases 1 + 2 Implementation
- **Task 1 DONE**: Failed attachment retry with `failedAttachments sync.Map` + `maxAttachmentRetries=3` cap. Fixed `uploaded.Add(1)` counter + `donePorals` typo.
- **Task 2 iteration 1**: External chunking with `HasMore: true` — BROKEN. bridgev2 only calls `FetchMessages` once for forward, so only first 5k of 27k messages delivered.
- **Task 2 iteration 2**: Reverted to single-batch — rejected by user, still needs chunking.
- **Task 2 iteration 3 (FINAL)**: Internal chunking loop within `FetchMessages`. Loops over `listOldestMessages`/`listForwardMessages` in 5k-row chunks, accumulates all messages into one response.
- **Task 3 DONE**: `cloud_attachment_cache` pruning using `json_each`/`json_extract`. Added to `clearAllData`. Runs after bootstrap via `runPostSyncHousekeeping`.
- **Task 4 DONE**: Cancellable context from `stopChan`. Semaphore acquire checks `ctx.Done()` for prompt shutdown.
- **Task 5 iteration 1**: `deleteOrphanedMessages` deleted ALL rows without `cloud_chat` — BROKE DM portals (they legitimately have no `cloud_chat`).
- **Task 5 iteration 2 (FINAL)**: Restricted to `deleted=TRUE` rows only. Safe for DMs.

### Session 3 (2026-02-20) — Bugfixes + parallel pre-upload
- **Forward backfill hang fix**: Large portals hung because uncached attachments triggered sequential 90s CloudKit downloads during `compileBatchMessage`. Added `preUploadChunkAttachments` — parallel pre-downloads (up to 32 concurrent) per chunk BEFORE the conversion loop. All downloads become cache hits.
- **What's next**: Phase 3 (doc updates) or continue monitoring deployment.
