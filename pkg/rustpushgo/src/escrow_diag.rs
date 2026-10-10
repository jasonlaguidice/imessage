//! Read-only escrow-bottle diagnostic for "No escrow bottles found".
//!
//! `KeychainClient::get_viable_bottles()` (upstream
//! `third_party/rustpush-upstream/src/icloud/keychain.rs`, `get_viable_bottles`)
//! reports "no bottles" for several unrelated reasons, all of which surface as
//! the same `No escrow bottles found`:
//!
//! 1. Apple has no escrow bottle for this Apple account at all — no trusted
//!    device ever escrowed its keychain, i.e. iCloud Keychain (and with it
//!    Messages in iCloud) is off.
//! 2. Apple has only *partial* bottles: a device began escrow but never
//!    completed it.
//! 3. Apple reports valid bottles, but `get_viable_bottles` filters them all
//!    away before returning: it keeps only bottles whose id matches an
//!    `getrecords` escrow-record label *and* whose metadata plist parses into
//!    `EscrowMetadata`. A missing `passcodeGeneration` field makes that plist
//!    parse fail, and upstream's `.ok()?` drops the bottle silently — so the
//!    caller still sees "no bottles" even though Apple has valid ones.
//!
//! This module tells those cases apart. It performs two *reads* and nothing
//! else: `KeychainClient::get_viable_bottles()`, and a raw
//! `fetchViableBottles` Cuttlefish function invoke (the very call upstream's
//! *private* `invoke_cuttlefish` makes, replayed here from the glue). Nothing
//! here joins a clique, saves or deletes a record, or touches keychain state.

use log::{info, warn};
use prost::Message as _;

/// One-line human verdict for the (valid, partial, viable) counts.
///
/// `valid` / `partial` are Apple's counts straight out of `fetchViableBottles`;
/// `viable` is how many of those valid bottles rustpush's `get_viable_bottles`
/// could actually use (i.e. that matched an escrow record label AND whose
/// metadata plist decoded).
pub(crate) fn classify(valid: usize, partial: usize, viable: usize) -> &'static str {
    if valid == 0 && partial == 0 {
        "no escrow bottles on this Apple account: no trusted device has escrowed its keychain (turn on iCloud Keychain / Messages in iCloud on an iPhone or Mac with a passcode, then retry)"
    } else if valid == 0 {
        "only partial escrow bottles exist: a device started escrow but never completed it (sign in to iCloud on that device again or set its passcode, then retry)"
    } else if viable == 0 {
        "Apple holds valid bottles but none matched a parseable escrow record: likely an escrow metadata format rustpush can't read (e.g. missing passcodeGeneration, upstream fix G9) or a label mismatch"
    } else {
        "escrow bottles are available"
    }
}

/// Query Apple (read-only) for this login's escrow bottles and describe what
/// came back.
///
/// Never fails and never panics: an escaping upstream panic is contained by
/// [`crate::panic_guard::guard_panic`] and reported as text, because
/// `CloudKitOpenContainer::perform` → `perform_operations` still `.expect()`s
/// on a reply whose operation UUID is missing — and this runs on the error
/// path of a *failed* keychain join, where the last thing we want is a second
/// panic on top of the original failure.
///
/// The caller appends the result to its "No escrow bottles found" error; Go
/// also gets it verbatim through
/// `WrappedTokenProvider::diagnose_escrow_bottles`.
pub(crate) async fn diagnose<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
) -> String {
    crate::panic_guard::guard_panic("escrow diagnostic", async {
        // Anything Apple-side that went wrong, reported verbatim after the
        // verdict so the reader can separate "Apple has nothing" from "Apple
        // answered, but not with what rustpush needed".
        let mut errors: Vec<String> = Vec::new();

        // (a) How many bottles rustpush itself can use. A failure here says
        //     nothing about Apple's side, so note it and query Apple anyway.
        let viable = match keychain.get_viable_bottles().await {
            Ok(bottles) => bottles.len(),
            Err(e) => {
                warn!("escrow diagnostic: getrecords/viable lookup failed: {}", e);
                errors.push(format!("getrecords/viable error: {}", e));
                0
            }
        };

        // (b) How many bottles Apple actually holds. `fetchViableBottles` with
        //     filter=1 / empty metrics is exactly what upstream's private
        //     `invoke_cuttlefish` (keychain.rs:1209) sends for
        //     `get_viable_bottles`; it is private, so replay it here.
        let (valid, partial, valid_ids) = match keychain.get_container().await {
            Err(e) => {
                warn!("escrow diagnostic: no Cuttlefish container: {}", e);
                errors.push(format!("Cuttlefish error: {}", e));
                (0, 0, Vec::new())
            }
            Ok(container) => {
                let body = cloudkit_proto::CuttlefishFetchViableBottleRequest {
                    filter: Some(1),
                    metrics: Some(vec![]),
                };
                let op = rustpush::cloudkit::FunctionInvokeOperation::new(
                    "Cuttlefish".to_string(),
                    "fetchViableBottles".to_string(),
                    body.encode_to_vec(),
                );
                match container
                    .perform(&rustpush::cloudkit::CloudKitSession::new(), op)
                    .await
                {
                    Err(e) => {
                        warn!("escrow diagnostic: fetchViableBottles failed: {}", e);
                        errors.push(format!("Cuttlefish error: {}", e));
                        (0, 0, Vec::new())
                    }
                    Ok(body) => {
                        match cloudkit_proto::CuttlefishFetchViableBottleResponse::decode(
                            &body[..],
                        ) {
                            Err(e) => {
                                warn!(
                                    "escrow diagnostic: unreadable fetchViableBottles reply: {}",
                                    e
                                );
                                errors.push(format!("Cuttlefish error: {}", e));
                                (0, 0, Vec::new())
                            }
                            Ok(response) => {
                                // `id` is the escrow record label a bottle is
                                // looked up by; one without a label can never
                                // match a `getrecords` record either.
                                let ids = response
                                    .valid
                                    .iter()
                                    .filter_map(|b| b.id.clone())
                                    .collect::<Vec<_>>();
                                (response.valid.len(), response.partial.len(), ids)
                            }
                        }
                    }
                }
            }
        };

        info!(
            "escrow diagnostic: valid={} partial={} viable={} valid ids {:?}",
            valid, partial, viable, valid_ids
        );

        let suffix = if errors.is_empty() {
            String::new()
        } else {
            format!("; {}", errors.join("; "))
        };
        format!(
            "escrow diagnostic: valid={} partial={} viable={} — {}{}",
            valid,
            partial,
            viable,
            classify(valid, partial, viable),
            suffix,
        )
    })
    .await
    .unwrap_or_else(|| "escrow diagnostic panicked (contained)".to_string())
}

#[cfg(test)]
mod tests {
    use super::classify;

    #[test]
    fn no_bottles_at_all() {
        let verdict = classify(0, 0, 0);
        assert!(verdict.starts_with("no escrow bottles on this Apple account"));
        assert!(verdict.contains("turn on iCloud Keychain"));
    }

    #[test]
    fn only_partial_bottles() {
        let verdict = classify(0, 2, 0);
        assert!(verdict.starts_with("only partial escrow bottles exist"));
        assert!(verdict.contains("never completed it"));
    }

    #[test]
    fn valid_bottles_none_parseable() {
        let verdict = classify(3, 0, 0);
        assert!(verdict.starts_with("Apple holds valid bottles"));
        assert!(verdict.contains("passcodeGeneration"));
    }

    #[test]
    fn usable_bottles_available() {
        let verdict = classify(1, 0, 1);
        assert_eq!(verdict, "escrow bottles are available");
        // `partial` says nothing once at least one valid bottle is usable —
        // that is precisely the case where Messages in iCloud works.
        assert_eq!(classify(2, 1, 2), "escrow bottles are available");
    }
}
