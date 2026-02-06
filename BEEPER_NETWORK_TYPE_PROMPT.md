# Messages Appear as "Matrix Chats" on Beeper Android

## Problem

When using the bridge via Beeper, portal rooms show up as generic "Matrix chats" on Beeper Android instead of being recognized as iMessage conversations. This means they don't get the iMessage icon, grouping, or proper network badge.

## How Beeper Identifies Bridge Networks

Beeper uses the `m.bridge` or `com.beeper.bridge_type` state events in portal rooms to identify which network a chat belongs to. The bridgev2 framework sets these automatically based on what `GetName()` returns from the connector.

## Current Code

In `pkg/connector/connector.go`:

```go
func (c *IMConnector) GetName() bridgev2.BridgeName {
    return bridgev2.BridgeName{
        DisplayName:      "iMessage",
        NetworkURL:       "https://support.apple.com/messages",
        NetworkIcon:      "mxc://maunium.net/tManJEpANASZvDVzvRvhILdl",
        NetworkID:        "imessage",
        BeeperBridgeType: "imessage",
        DefaultPort:      29332,
    }
}
```

The `BeeperBridgeType: "imessage"` should tell Beeper this is an iMessage bridge. However, there may be additional requirements.

## Things to Investigate

1. **Check what state events bridgev2 actually sets on portal rooms.** Look at the mautrix-go bridgev2 source to see how `BridgeName` is used to set room state. Search for `BeeperBridgeType`, `m.bridge`, `com.beeper.bridge_type` in the mautrix-go dependency.

2. **Compare with the official mautrix-imessage bridge.** Look at what bridge state events the official Beeper imessage bridge sets. The `BeeperBridgeType` might need to be `"imessagego"` (the beeper-imessage variant) or there might be additional state events needed.

3. **Check if the bridge info version matters.** In `capabilities.go`:
   ```go
   func (c *IMConnector) GetBridgeInfoVersion() (info, capabilities int) {
       return 1, 1
   }
   ```
   Beeper might expect specific version numbers.

4. **Check the appservice registration.** When bbctl generates the config with `--type bridgev2`, it might set a bridge type that doesn't match what our connector reports. Look at the generated registration YAML for any `bridge_type` or similar fields.

5. **Check if Beeper Android specifically looks for certain room state keys or account data.** The issue might be client-side — Beeper Android might only recognize bridges that were registered via `bbctl run` with a known type identifier.

6. **Search for how other custom bridgev2 bridges handle this.** Check mautrix-go source for how `BeeperBridgeType` flows into room state events, and whether there are any additional calls needed (like `SendBridgeInfo` or similar).

## Files to Investigate

- `pkg/connector/connector.go` — `GetName()` return values
- mautrix-go bridgev2 source (in `~/go/pkg/mod/maunium.net/go/mautrix@*/bridgev2/`) — search for `BeeperBridgeType`, `bridge_type`, `m.bridge`
- The bbctl-generated config at `~/.local/share/bbctl/sh-imessage/config.yaml` — check for bridge type fields

## Fix

Determine exactly what state events / metadata Beeper Android needs to recognize a bridge as iMessage, and ensure our connector sets them correctly. This might be as simple as adjusting `BeeperBridgeType` or as involved as adding custom state events to portal rooms.

After fixing, `make build` must succeed.
