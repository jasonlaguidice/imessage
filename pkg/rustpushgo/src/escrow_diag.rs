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

use std::collections::{BTreeSet, HashMap};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use log::{info, warn};
use prost::Message as _;
use serde::Serialize;

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
/// The caller appends the result to its "No escrow bottles found" error; Go
/// also gets it verbatim through
/// `WrappedTokenProvider::diagnose_escrow_bottles`.
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

/// The exact request upstream's `get_viable_bottles` sends to
/// `/escrowproxy/api/get_records`. Upstream's `EscrowRequest` (keychain.rs:631)
/// carries several `Option` fields that are all `None` for `get_records` and
/// that the plist XML serializer omits; we therefore carry only the fields the
/// command actually populates. `EscrowCommand::Getrecords` serialises to the
/// string `"GETRECORDS"` (SCREAMING_SNAKE_CASE of `Getrecords`).
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct GetRecordsRequest {
    command: String,
    label: String,
    #[serde(rename = "transactionUUID")]
    transaction_uuid: String,
    user_action_label: String,
    version: u32,
}

/// Build the escrow `get_records` POST body: the same plist XML string upstream
/// produces via its private `plist_to_string` (an XML serialisation of the
/// request). Returns the UTF-8 body bytes.
fn get_records_request_body() -> Result<Vec<u8>, String> {
    let request = GetRecordsRequest {
        command: "GETRECORDS".to_string(),
        label: "com.apple.securebackup.record".to_string(),
        transaction_uuid: uuid::Uuid::new_v4().to_string().to_uppercase(),
        user_action_label: "cdpd: unknown activity".to_string(),
        version: 1,
    };
    let mut bytes: Vec<u8> = Vec::new();
    plist::to_writer_xml(&mut bytes, &request).map_err(|e| format!("xml encode: {e}"))?;
    Ok(bytes)
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
    let body = match get_records_request_body() {
        Ok(b) => b,
        Err(e) => return e,
    };

    // Auth headers: `auth`/`email` come from the token provider exactly as
    // upstream's `invoke_escrow` does.
    let auth = keychain
        .token_provider
        .get_gsa_token("com.apple.gs.idms.pet")
        .await
        .unwrap_or_default();
    let email = keychain
        .token_provider
        .get_gsa_email()
        .await
        .unwrap_or_default();

    // Anisette + OSConfig headers mirror upstream's `get_escrow_headers`
    // (keychain.rs:2122).
    let anisette_headers: HashMap<String, String> = {
        let mut guard = keychain.anisette.lock().await;
        match guard.get_headers().await {
            Ok(h) => h.clone(),
            Err(e) => return format!("anisette headers unavailable: {e}"),
        }
    };
    let mut headers: HashMap<String, String> = HashMap::new();
    headers.insert(
        "User-Agent".to_string(),
        keychain
            .config
            .get_normal_ua("com.apple.sbd/638.100.48"),
    );
    headers.insert("Accept-Language".to_string(), "en-US,en;q=0.9".to_string());
    headers.insert("x-apple-i-device-type".to_string(), "1".to_string());
    headers.insert("Accept".to_string(), "*/*".to_string());
    headers.insert("X-Apple-I-Locale".to_string(), "en_US".to_string());
    for (k, v) in anisette_headers {
        headers.insert(k, v);
    }
    let iphone = headers
        .get("X-Mme-Client-Info")
        .map(|s| s.contains("iPhone OS"))
        .unwrap_or(false);
    headers.insert(
        "X-Mme-Client-Info".to_string(),
        keychain
            .config
            .get_adi_mme_info("com.apple.AuthKit/1 (com.apple.sbd/638.100.48)", !iphone),
    );

    let url = format!(
        "{}/escrowproxy/api/get_records",
        host.trim_end_matches('/')
    );

    // rustpush's own `REQWEST` client is private (`mod util`), so build an
    // equivalent one: rustls with Apple's root CAs added (on top of this
    // crate's webpki roots) and title-case HTTP/1 headers, as upstream does.
    let mut builder = reqwest::Client::builder()
        .use_rustls_tls()
        .http1_title_case_headers();
    for der in [
        &include_bytes!("../../../third_party/rustpush-upstream/certs/root/AppleIncRootCertificate.cer")[..],
        &include_bytes!("../../../third_party/rustpush-upstream/certs/root/AppleRootCA-G2.cer")[..],
        &include_bytes!("../../../third_party/rustpush-upstream/certs/root/AppleRootCA-G3.cer")[..],
    ] {
        match reqwest::Certificate::from_der(der) {
            Ok(cert) => builder = builder.add_root_certificate(cert),
            Err(e) => return format!("failed to load Apple root certificate: {e}"),
        }
    }
    let client = match builder.build() {
        Ok(c) => c,
        Err(e) => return format!("failed to build escrow HTTP client: {e}"),
    };

    let mut request = client
        .post(&url)
        .header("Content-Type", "application/x-apple-plst")
        .basic_auth(&email, Some(&auth));
    for (k, v) in &headers {
        request = request.header(k.as_str(), v.as_str());
    }
    let request = request.body(body);

    let response = match request.send().await {
        Ok(r) => r,
        Err(e) => return format!("get_records request failed: {e}"),
    };
    let status = response.status();
    let resp_bytes = match response.bytes().await {
        Ok(b) => b,
        Err(e) => return format!("failed to read get_records body: {e}"),
    };

    if !status.is_success() {
        let detail = match plist::from_bytes::<plist::Value>(&resp_bytes) {
            Ok(v) => format!("{v:?}"),
            Err(_) => format!("(non-plist body, {} bytes)", resp_bytes.len()),
        };
        return format!("get_records returned HTTP {status}: {detail}");
    }

    let value: plist::Value = match plist::from_bytes(&resp_bytes) {
        Ok(v) => v,
        Err(e) => return format!("failed to parse get_records plist: {e}"),
    };

    // The response is a dictionary with a `metadataList` array of
    // `{label, metadata(base64 plist)}` (camelCase of the private
    // `metadata_list` field upstream deserialises).
    let metadata_list = match value
        .as_dictionary()
        .and_then(|d| d.get("metadataList"))
    {
        Some(v) => v,
        None => return "get_records returned no `metadataList`".to_string(),
    };
    let arr = match metadata_list.as_array() {
        Some(a) => a,
        None => {
            return format!("`metadataList` is not an array (got {metadata_list:?})")
        }
    };

    let mut records: Vec<(String, Option<plist::Value>)> = Vec::new();
    for entry in arr {
        let Some(dict) = entry.as_dictionary() else {
            records.push(("?".to_string(), None));
            continue;
        };
        let label = dict
            .get("label")
            .and_then(|v| v.as_string())
            .unwrap_or_default()
            .to_string();
        let metadata = match dict.get("metadata").and_then(|v| v.as_string()) {
            Some(b64) => match BASE64_STANDARD.decode(b64) {
                Ok(bytes) => plist::from_bytes::<plist::Value>(&bytes).ok(),
                Err(_) => None,
            },
            None => None,
        };
        records.push((label, metadata));
    }

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
                    Err(e) => format!("parse error: {e}"),
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