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
//! This module tells those cases apart. It performs *reads* and nothing else:
//! `KeychainClient::get_viable_bottles()`, a raw `fetchViableBottles`
//! Cuttlefish function invoke (the very call upstream's *private*
//! `invoke_cuttlefish` makes, replayed here from the glue), and — when valid
//! bottles exist but none are viable — a raw `get_records` escrow POST (the
//! very call upstream's *private* `invoke_escrow` makes for
//! `EscrowCommand::Getrecords`, replayed here from the glue) so the
//! label-mismatch vs. metadata-parse-failure cause can be named. Nothing here
//! joins a clique, saves or deletes a record, or touches keychain state.

use std::collections::BTreeSet;

use log::{info, warn};

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
/// `escrow_host` is this login's escrow-proxy host (`escrowProxyUrl`) from the
/// MobileMe delegate's `com.apple.Dataclass.KeychainSync` class. It is only
/// used to replay the `get_records` read when Apple reports valid bottles but
/// rustpush could use none of them, and to name *which* filter (label match
/// vs. metadata parse) dropped them. `None` means we could not read it.
///
/// The caller appends the result to its "No escrow bottles found" error.
pub(crate) async fn diagnose<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
    escrow_host: Option<String>,
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
        let (valid, partial, valid_ids) =
            match crate::escrow_bottles::fetch_cuttlefish_bottles(keychain).await {
                Err(e) => {
                    warn!("escrow diagnostic: {}", e);
                    errors.push(format!("Cuttlefish error: {}", e));
                    (0, 0, Vec::new())
                }
                Ok(bottles) => {
                    // `id` is the escrow record label a bottle is looked up by;
                    // one without a label can never match a record either.
                    let ids = bottles
                        .valid
                        .iter()
                        .filter_map(|b| b.id.clone())
                        .collect::<Vec<_>>();
                    (bottles.valid.len(), bottles.partial, ids)
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
        let verdict = format!(
            "escrow diagnostic: valid={} partial={} viable={} — {}{}",
            valid,
            partial,
            viable,
            classify(valid, partial, viable),
            suffix,
        );

        // (c) When Apple holds valid bottles but rustpush can use none, replay
        //     the `get_records` escrow read upstream's `get_viable_bottles`
        //     performs, so we can name whether the valid ids matched a record
        //     label at all and whether the metadata plist is decodable.
        let records_suffix = if valid > viable {
            match escrow_host {
                Some(host) => {
                    format!("; records: {}", records_report(keychain, &host, &valid_ids).await)
                }
                None => "; records: escrow host unknown".to_string(),
            }
        } else {
            String::new()
        };

        format!("{verdict}{records_suffix}")
    })
    .await
    .unwrap_or_else(|| "escrow diagnostic panicked (contained)".to_string())
}

/// Replay the `get_records` escrow read that upstream's `get_viable_bottles`
/// performs (its *private* `invoke_escrow` with `EscrowCommand::Getrecords`),
/// and summarise how the escrow records line up against the valid bottle ids.
///
/// Read-only and total: every failure becomes report text, never a panic or a
/// propagated error. The caller invokes this only inside
/// [`crate::panic_guard::guard_panic`].
///
/// For `get_viable_bottles` to keep a bottle, a `get_records` record's `label`
/// must equal the bottle's `id` AND the record's base64 `metadata` must decode
/// to a plist that parses into `EscrowMetadata`. This function separates those
/// two conditions so "valid but not viable" stops meaning one thing.
async fn records_report<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
    host: &str,
    valid_ids: &[String],
) -> String {
    let records = match crate::escrow_bottles::fetch_escrow_records(keychain, host).await {
        Ok(r) => r,
        Err(e) => return e,
    };

    let summary = summarize_records(&records, valid_ids);
    info!("escrow diagnostic: records report: {summary}");
    summary
}

/// Pure summary of `get_records` output against the valid bottle ids: which
/// valid ids matched a record label at all, and for each match whether the
/// metadata plist parses into `EscrowMetadata` (naming the offending field on
/// failure).
///
/// `records` is a slice of `(label, decoded-metadata-or-None)`; `None` means
/// the base64 `metadata` was absent or did not decode to a plist. `valid_ids`
/// is the list of bottle ids from `fetchViableBottles` (at most the first five
/// are reported). This is the matching/keys/parse-attempt part only, split out
/// from [`records_report`] so it is unit-testable without a network.
fn summarize_records(records: &[(String, Option<plist::Value>)], valid_ids: &[String]) -> String {
    let mut parts: Vec<String> = vec![format!("records={}", records.len())];

    let mut any_match = false;
    for id in valid_ids.iter().take(5) {
        match records.iter().find(|(label, _)| label == id) {
            Some((_, Some(meta))) => {
                any_match = true;
                let keys: BTreeSet<String> = meta
                    .as_dictionary()
                    .map(|d| d.keys().cloned().collect())
                    .unwrap_or_default();
                let key_names: Vec<String> = keys.iter().cloned().collect();
                let key_list = if key_names.is_empty() {
                    "(no top-level keys)".to_string()
                } else {
                    key_names.join(",")
                };
                let parse = match plist::from_value::<rustpush::keychain::EscrowMetadata>(meta) {
                    Ok(_) => "parse ok".to_string(),
                    Err(e) => {
                        let fallback = if crate::escrow_bottles::parse_metadata_lenient(meta).is_ok() {
                            " (glue fallback can read it)"
                        } else {
                            ""
                        };
                        format!("parse error: {e}{fallback}")
                    }
                };
                parts.push(format!("{id}: match [keys: {key_list}] {parse}"));
            }
            Some((_, None)) => {
                any_match = true;
                parts.push(format!("{id}: match (metadata missing or not a plist)"));
            }
            None => {
                parts.push(format!("{id}: no-record"));
            }
        }
    }

    if !any_match && !valid_ids.is_empty() {
        // Nothing matched a valid id; show the labels Apple actually returned
        // (bottle UUID-like ids, not secrets) so the mismatch is visible.
        let labels: Vec<&str> = records.iter().take(5).map(|(l, _)| l.as_str()).collect();
        if labels.is_empty() {
            parts.push("(no record labels returned)".to_string());
        } else {
            parts.push(format!(
                "record labels: {}",
                labels.join(", ")
            ));
        }
    }

    parts.join(" | ")
}

#[cfg(test)]
mod tests {
    use super::classify;
    use super::summarize_records;
    use plist::{Dictionary, Value};

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

    /// A full `EscrowMetadata` dictionary (all the fields upstream expects).
    fn valid_meta_dict() -> Value {
        let mut dict = Dictionary::new();
        dict.insert("serial".into(), Value::String("S1".into()));
        dict.insert("build".into(), Value::String("B1".into()));
        dict.insert("passcodeGeneration".into(), Value::Integer(13.into()));
        dict.insert(
            "com.apple.securebackup.timestamp".into(),
            Value::String("2020-01-01 00:00:00".into()),
        );
        dict.insert("bottleID".into(), Value::String("BOTTLE".into()));
        dict.insert("ClientMetadata".into(), Value::Dictionary(Dictionary::new()));
        dict.insert("escrowedSPKI".into(), Value::Data(vec![1, 2, 3]));
        dict.insert(
            "SecureBackupUsesMultipleiCSCs".into(),
            Value::Boolean(true),
        );
        Value::Dictionary(dict)
    }

    /// A full `EscrowMetadata` dictionary missing the given field.
    fn meta_dict_missing(field: &str) -> Value {
        let mut value = valid_meta_dict();
        value.as_dictionary_mut().unwrap().remove(field);
        value
    }

    #[test]
    fn summarize_matching_record_missing_field_reports_parse_error() {
        let id = "BOTTLE-1".to_string();
        let meta = meta_dict_missing("passcodeGeneration");
        let records = vec![(id.clone(), Some(meta))];
        let valid = vec![id];
        let summary = summarize_records(&records, &valid);
        assert!(summary.contains("match"), "summary: {summary}");
        assert!(summary.contains("parse error"), "summary: {summary}");
        assert!(
            summary.contains("passcodeGeneration"),
            "summary should name the missing field: {summary}"
        );
        assert!(!summary.contains("parse ok"), "summary: {summary}");
    }

    #[test]
    fn summarize_notes_when_glue_fallback_can_read_the_record() {
        let id = "BOTTLE-3".to_string();
        let records = vec![(id.clone(), Some(meta_dict_missing("passcodeGeneration")))];
        let summary = summarize_records(&records, &[id]);
        assert!(summary.contains("(glue fallback can read it)"), "summary: {summary}");
        let records = vec![("BOTTLE-4".to_string(), Some(meta_dict_missing("serial")))];
        let summary = summarize_records(&records, &["BOTTLE-4".to_string()]);
        assert!(!summary.contains("glue fallback"), "summary: {summary}");
    }

    #[test]
    fn summarize_matching_record_presents_keys_and_parses_ok() {
        let id = "BOTTLE-2".to_string();
        let records = vec![(id.clone(), Some(valid_meta_dict()))];
        let valid = vec![id];
        let summary = summarize_records(&records, &valid);
        assert!(summary.contains("parse ok"), "summary: {summary}");
        assert!(
            summary.contains("passcodeGeneration"),
            "summary lists keys: {summary}"
        );
        assert!(
            summary.contains("ClientMetadata"),
            "summary lists keys: {summary}"
        );
    }

    #[test]
    fn summarize_no_matching_record_lists_labels() {
        let records = vec![
            ("AAA".to_string(), Some(valid_meta_dict())),
            ("BBB".to_string(), Some(valid_meta_dict())),
        ];
        let valid = vec!["111".to_string(), "222".to_string()];
        let summary = summarize_records(&records, &valid);
        assert!(summary.contains("111: no-record"), "summary: {summary}");
        assert!(summary.contains("222: no-record"), "summary: {summary}");
        assert!(
            summary.contains("record labels:"),
            "summary: {summary}"
        );
        assert!(summary.contains("AAA"), "summary: {summary}");
        assert!(summary.contains("BBB"), "summary: {summary}");
        // No valid id actually matched a label.
        assert!(
            !summary.contains("111: match") && !summary.contains("222: match"),
            "summary: {summary}"
        );
    }

    #[test]
    fn summarize_mixed_matches_and_missing() {
        let records = vec![
            ("AAA".to_string(), Some(meta_dict_missing("serial"))),
            ("BBB".to_string(), Some(valid_meta_dict())),
            ("CCC".to_string(), None),
        ];
        let valid = vec![
            "AAA".to_string(),
            "BBB".to_string(),
            "CCC".to_string(),
            "DDD".to_string(),
        ];
        let summary = summarize_records(&records, &valid);
        assert!(summary.contains("AAA: match"), "summary: {summary}");
        assert!(summary.contains("BBB: match"), "summary: {summary}");
        assert!(
            summary.contains("CCC: match (metadata missing or not a plist)"),
            "summary: {summary}"
        );
        assert!(summary.contains("DDD: no-record"), "summary: {summary}");
        assert!(summary.contains("parse ok"), "summary: {summary}");
        assert!(summary.contains("parse error"), "summary: {summary}");
    }
}