# 2FA Push Notification Not Arriving — Debug Prompt

## Problem

When a user logs in via the Matrix bot (`login` command), they enter Apple ID + password, the login succeeds (SRP auth completes), and the bridge correctly identifies that 2FA is needed. However, the **2FA push notification never arrives on the user's trusted devices**. The user had to manually retrieve a code from OpenBubbles as a workaround.

The bridge prompts for the 2FA code and accepts it fine — the issue is solely that Apple's trusted device push isn't firing.

## Reproduction

1. DM the bridge bot and send `login`
2. Enter Apple ID and password
3. Bridge says "2FA required" and prompts for code
4. **Expected**: 2FA code popup appears on trusted Apple devices (iPhone, iPad, Mac)
5. **Actual**: No popup appears. Code never arrives.

This was reported by a user on an M1 Mac Mini running via Beeper (bbctl).
On my own machine (Apple Silicon, macOS 26.1, standalone setup), 2FA works correctly.

## Relevant Code

### Rust FFI login flow: `pkg/rustpushgo/src/lib.rs` lines ~488-548

The `login_start` function:
1. Hashes the password with SHA-256
2. Creates an `AppleAccount` with anisette provider
3. Calls `account.login_email_pass()` which does GSA SRP authentication
4. Matches on the returned `LoginState`:
   - If `NeedsDevice2FA` or `NeedsSMS2FA`: calls `account.send_2fa_to_devices().await`
   - If `Needs2FAVerification`: does NOT call `send_2fa_to_devices()` (assumes push already sent)
5. Returns the `LoginSession` for the Go side to prompt for the code

**Potential issue**: The `send_2fa_to_devices()` result is silently discarded with `let _ = ...`. If that HTTP request fails, we'd never know.

### icloud-auth `send_2fa_to_devices`: `rustpush/apple-private-apis/icloud-auth/src/client.rs` line ~765

```rust
pub async fn send_2fa_to_devices(&self) -> Result<LoginState, crate::Error> {
    let headers = self.build_2fa_headers(false);
    let res = self.client
        .get("https://gsa.apple.com/auth/verify/trusteddevice")
        .headers(headers.await?)
        .send().await?;
    if !res.status().is_success() {
        return Err(Error::AuthSrp);
    }
    return Ok(LoginState::Needs2FAVerification);
}
```

### icloud-auth `login_email_pass` return states: same file, line ~736

The `au` field in the SRP response determines the state:
- `"trustedDeviceSecondaryAuth"` → `NeedsDevice2FA`
- `"secondaryAuth"` → `NeedsSMS2FA`
- (missing `au` field) → `LoggedIn` (no 2FA needed)

There's also `Needs2FAVerification` which is returned by `send_2fa_to_devices()` itself — but the initial `login_email_pass` never returns that state directly.

### icloud-auth's own `login_with_anisette` (reference): same file, line ~280

The upstream library's own high-level login function handles the 2FA loop differently:
```rust
LoginState::NeedsSMS2FA | LoginState::NeedsDevice2FA => {
    _self.send_2fa_to_devices().await?;
    // ... then prompts for code and calls verify_2fa
}
```

Note it propagates the error with `?` rather than discarding with `let _ =`.

## Things to Investigate

1. **Is `send_2fa_to_devices()` actually failing silently?** The `let _ = account.send_2fa_to_devices().await;` discards the Result. Log the error if it fails.

2. **Is the `LoginState` actually `NeedsDevice2FA`?** Or is it `Needs2FAVerification` (which skips the `send_2fa_to_devices` call)? Add logging to show which exact state was returned.

3. **Are the 2FA headers correct?** `build_2fa_headers` constructs headers for the trusteddevice endpoint. Check if anisette/session state is properly carried over.

4. **Is there a timing issue?** Maybe `send_2fa_to_devices` needs to be called before returning to the Go side. Currently it's fire-and-forget in a match arm.

5. **Does the anisette provider matter?** We use `default_provider` (local omnisette). The Beeper user might have different anisette behavior. Check if the anisette state directory (`state/anisette`) exists and is writable from the bbctl working directory.

6. **Does OpenBubbles do something different?** The user said they got the code from OpenBubbles — check if OpenBubbles calls a different endpoint or uses SMS 2FA as fallback.

## Files to Edit

- `pkg/rustpushgo/src/lib.rs` — the `login_start` function (~line 488)
- `rustpush/apple-private-apis/icloud-auth/src/client.rs` — `send_2fa_to_devices` (~line 765) and `build_2fa_headers`

## Fix Approach

At minimum:
1. **Log the actual LoginState** returned by `login_email_pass` so we can see which 2FA path is taken
2. **Don't discard the `send_2fa_to_devices` result** — log the error if it fails
3. **Consider always calling `send_2fa_to_devices`** regardless of the exact LoginState variant (as long as it's any 2FA state), since it's idempotent

After fixing, rebuild with `make build` and verify the 2FA popup arrives on a test login.
