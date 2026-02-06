# Read and Delivery Receipts Not Working

## Problem

A user reports that read and delivery receipts don't work when using the bridge via Beeper. Typing indicators work ("sort of"), but read receipts and delivery receipts do not.

Two directions to investigate:
- **Incoming** (iMessage → Matrix): When someone reads your message in iMessage, the read receipt should appear in Matrix
- **Outgoing** (Matrix → iMessage): When you read a message in Matrix, a read receipt should be sent to iMessage

## Relevant Code

### Incoming receipts (iMessage → Matrix)

In `pkg/rustpushgo/src/lib.rs`, the receive loop handles messages from rustpush:
- `Message::Read` → sets `is_read_receipt = true` on the wrapped message
- `Message::Delivered` → sets `is_delivered = true`

In `pkg/connector/client.go`:
- `OnMessage()` checks `msg.IsDelivered` and returns early (line ~170) — **delivery receipts are silently dropped, never forwarded to Matrix**
- `OnMessage()` checks `msg.IsReadReceipt` and calls `handleReadReceipt()` which queues a `simplevent.Receipt` with type `RemoteEventReadReceipt`

**Bug found**: Delivery receipts (`msg.IsDelivered`) are dropped with a bare `return`. They're never forwarded to Matrix. This means the sender never sees "Delivered" status.

### Outgoing receipts (Matrix → iMessage)

In `pkg/connector/client.go`:
- `HandleMatrixReadReceipt()` calls `c.client.SendReadReceipt(conv, c.handle)`
- This sends `Message::Read` via rustpush

In `pkg/rustpushgo/src/lib.rs`:
- `send_read_receipt()` creates a `MessageInst` with `Message::Read` and sends it

### Auto-delivery receipts

In `OnMessage()`, there's logic to send a delivery receipt back when `msg.SendDelivered` is true:
```go
if msg.SendDelivered && msg.Sender != nil && !msg.IsDelivered && !msg.IsReadReceipt {
    go func() {
        conv := c.makeConversation(msg.Participants, msg.GroupName)
        if err := c.client.SendReadReceipt(conv, c.handle); err != nil {
            log.Warn().Err(err).Msg("Failed to send delivery receipt")
        }
    }()
}
```

**Bug**: This sends a `ReadReceipt` (Message::Read) instead of a delivery acknowledgment. The iMessage protocol distinguishes between "delivered" and "read" — sending Read when you mean Delivered would mark messages as read immediately on the sender's device.

## Things to Investigate

1. **Are incoming read receipts reaching the Rust receive loop?** Add logging in the receive loop for `Message::Read` and `Message::Delivered` to confirm they arrive.

2. **Is `handleReadReceipt` creating valid events?** The `simplevent.Receipt` might be missing required fields for Beeper's hungryserv. Check if a `LastMessage` field or similar is needed.

3. **Does Beeper/hungryserv support `RemoteEventReadReceipt`?** Some Matrix implementations handle receipts differently. Check if the event is being queued but rejected by the homeserver.

4. **Delivery vs Read distinction**: rustpush has both `Message::Delivered` and `Message::Read`. Currently we only bridge Read receipts. We should also bridge Delivered receipts (at least as a delivery confirmation).

5. **The auto-delivery response sends Read instead of Delivered**: The `SendDelivered` flag handling calls `SendReadReceipt` which sends `Message::Read`. We may need a separate `SendDelivered` method on the Rust side that sends `Message::Delivered` instead.

## Files to Edit

- `pkg/connector/client.go` — `OnMessage()`, `handleReadReceipt()`, delivery receipt handling
- `pkg/rustpushgo/src/lib.rs` — receive loop (check if Read/Delivered messages pass the `has_payload()` filter), potentially add a `send_delivered` method

## Fix Approach

1. **Don't silently drop delivery receipts** — forward them to Matrix as delivery confirmations
2. **Fix the auto-delivery response** — send Delivered, not Read, when `SendDelivered` is true. This likely requires adding a `send_delivered()` method to the Rust Client that sends `Message::Delivered` instead of `Message::Read`
3. **Add logging** for all receipt types (read, delivered) in both directions
4. Test both directions: send a message from Matrix → verify "Delivered" and "Read" appear; receive a message → verify read receipt is sent back when read in Matrix

After fixing, `make build` must succeed.
