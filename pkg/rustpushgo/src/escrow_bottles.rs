//! Escrow bottle access with a glue-side fallback for escrow records rustpush
//! can't parse.
//!
//! rustpush's `KeychainClient::get_viable_bottles()` keeps a valid Cuttlefish
//! bottle only if an escrow `get_records` record carries its label AND that
//! record's metadata plist deserialises into `EscrowMetadata`. Records written
//! by current Apple devices lack `passcodeGeneration`, which `EscrowMetadata`
//! requires (OpenBubbles/rustpush#23, unmerged), so every bottle is dropped and
//! the keychain join reports "No escrow bottles found". Recovery itself
//! (`join_clique_from_escrow`) uses only the bottle, never that field.
//!
//! [`viable_bottles`] asks rustpush first and only falls back to its own
//! read-only Cuttlefish + escrow reads when rustpush returns nothing, parsing
//! the metadata leniently. Once rustpush reads these records itself, the
//! fallback is never taken. No rustpush source change.

use std::collections::HashMap;

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use log::{info, warn};
use prost::Message as _;
use serde::Serialize;

/// Apple's Cuttlefish view of this account's escrow bottles.
pub(crate) struct CuttlefishBottles {
    pub valid: Vec<cloudkit_proto::EscrowData>,
    pub partial: usize,
}

/// `fetchViableBottles` with filter=1 / empty metrics: exactly what upstream's
/// private `invoke_cuttlefish` sends for `get_viable_bottles`, replayed here.
pub(crate) async fn fetch_cuttlefish_bottles<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
) -> Result<CuttlefishBottles, String> {
    let container = keychain
        .get_container()
        .await
        .map_err(|e| format!("no Cuttlefish container: {e}"))?;
    let body = cloudkit_proto::CuttlefishFetchViableBottleRequest {
        filter: Some(1),
        metrics: Some(vec![]),
    };
    let op = rustpush::cloudkit::FunctionInvokeOperation::new(
        "Cuttlefish".to_string(),
        "fetchViableBottles".to_string(),
        body.encode_to_vec(),
    );
    let reply = container
        .perform(&rustpush::cloudkit::CloudKitSession::new(), op)
        .await
        .map_err(|e| format!("fetchViableBottles failed: {e}"))?;
    let response = cloudkit_proto::CuttlefishFetchViableBottleResponse::decode(&reply[..])
        .map_err(|e| format!("unreadable fetchViableBottles reply: {e}"))?;
    Ok(CuttlefishBottles {
        partial: response.partial.len(),
        valid: response.valid,
    })
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

/// Replay the escrow `get_records` read upstream's `get_viable_bottles` makes
/// (its private `invoke_escrow` with `EscrowCommand::Getrecords`): same URL,
/// headers, auth and body. Returns `(label, decoded metadata plist)` per
/// record; `None` when a record's metadata is absent or not a plist.
pub(crate) async fn fetch_escrow_records<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
    host: &str,
) -> Result<Vec<(String, Option<plist::Value>)>, String> {
    let body = get_records_request_body()?;

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
            Err(e) => return Err(format!("anisette headers unavailable: {e}")),
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
            Err(e) => return Err(format!("failed to load Apple root certificate: {e}")),
        }
    }
    let client = match builder.build() {
        Ok(c) => c,
        Err(e) => return Err(format!("failed to build escrow HTTP client: {e}")),
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
        Err(e) => return Err(format!("get_records request failed: {e}")),
    };
    let status = response.status();
    let resp_bytes = match response.bytes().await {
        Ok(b) => b,
        Err(e) => return Err(format!("failed to read get_records body: {e}")),
    };

    if !status.is_success() {
        let detail = match plist::from_bytes::<plist::Value>(&resp_bytes) {
            Ok(v) => format!("{v:?}"),
            Err(_) => format!("(non-plist body, {} bytes)", resp_bytes.len()),
        };
        return Err(format!("get_records returned HTTP {status}: {detail}"));
    }

    let value: plist::Value = match plist::from_bytes(&resp_bytes) {
        Ok(v) => v,
        Err(e) => return Err(format!("failed to parse get_records plist: {e}")),
    };

    // The response is a dictionary with a `metadataList` array of
    // `{label, metadata(base64 plist)}` (camelCase of the private
    // `metadata_list` field upstream deserialises).
    let metadata_list = match value
        .as_dictionary()
        .and_then(|d| d.get("metadataList"))
    {
        Some(v) => v,
        None => return Err("get_records returned no `metadataList`".to_string()),
    };
    let arr = match metadata_list.as_array() {
        Some(a) => a,
        None => {
            return Err(format!("`metadataList` is not an array (got {metadata_list:?})"))
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

    Ok(records)
}

/// Parse an escrow record's metadata into `EscrowMetadata`, defaulting
/// `passcodeGeneration` to 0 when the record omits it. That field is only
/// consulted when enrolling a new bottle, never for recovery.
pub(crate) fn parse_metadata_lenient(
    meta: &plist::Value,
) -> Result<rustpush::keychain::EscrowMetadata, String> {
    let Some(dict) = meta.as_dictionary() else {
        return Err("escrow metadata is not a dictionary".to_string());
    };
    let mut patched = dict.clone();
    if !patched.contains_key("passcodeGeneration") {
        patched.insert("passcodeGeneration".to_string(), plist::Value::Integer(0.into()));
    }
    plist::from_value::<rustpush::keychain::EscrowMetadata>(&plist::Value::Dictionary(patched))
        .map_err(|e| e.to_string())
}

/// Pair Apple's valid bottles with leniently parsed escrow records, in Apple's
/// order. Matching mirrors rustpush: a record's label equals the bottle id
/// (the prost getter, "" when absent).
fn pair_bottles(
    valid: &[cloudkit_proto::EscrowData],
    records: &[(String, Option<plist::Value>)],
) -> Vec<(cloudkit_proto::EscrowData, rustpush::keychain::EscrowMetadata)> {
    let mut out = Vec::new();
    for bottle in valid {
        let Some((_, Some(meta))) = records.iter().find(|(label, _)| label == bottle.id()) else {
            continue;
        };
        match parse_metadata_lenient(meta) {
            Ok(parsed) => out.push((bottle.clone(), parsed)),
            Err(e) => warn!("Escrow fallback: skipping bottle {}: {}", bottle.id(), e),
        }
    }
    out
}

/// The bottles a keychain join can use: rustpush's own answer when it has
/// one, otherwise the glue-side fallback described in the module docs.
/// Fallback failures are logged and yield an empty list, so callers keep
/// reporting "No escrow bottles found" (with the diagnostic) as before.
pub(crate) async fn viable_bottles<P: omnisette::AnisetteProvider + Send + Sync + 'static>(
    keychain: &rustpush::keychain::KeychainClient<P>,
    escrow_host: Option<String>,
) -> Result<Vec<(cloudkit_proto::EscrowData, rustpush::keychain::EscrowMetadata)>, String> {
    let bottles = keychain.get_viable_bottles().await.map_err(|e| e.to_string())?;
    if !bottles.is_empty() {
        return Ok(bottles);
    }
    let Some(host) = escrow_host else {
        return Ok(Vec::new());
    };
    let fallback = crate::panic_guard::guard_panic("escrow bottle fallback", async {
        let cuttlefish = match fetch_cuttlefish_bottles(keychain).await {
            Ok(c) => c,
            Err(e) => {
                warn!("Escrow fallback: {}", e);
                return Vec::new();
            }
        };
        if cuttlefish.valid.is_empty() {
            return Vec::new();
        }
        let records = match fetch_escrow_records(keychain, &host).await {
            Ok(r) => r,
            Err(e) => {
                warn!("Escrow fallback: {}", e);
                return Vec::new();
            }
        };
        pair_bottles(&cuttlefish.valid, &records)
    })
    .await
    .unwrap_or_default();
    if !fallback.is_empty() {
        info!(
            "Escrow fallback: rustpush could not read {} escrow record(s) (missing passcodeGeneration?); using glue-parsed metadata",
            fallback.len()
        );
    }
    Ok(fallback)
}

#[cfg(test)]
mod tests {
    use super::{pair_bottles, parse_metadata_lenient};
    use plist::{Dictionary, Value};

    /// Shaped like a live record: all EscrowMetadata fields, plus the extra
    /// keys Apple sends, optionally without passcodeGeneration.
    fn record(with_generation: bool) -> Value {
        let mut dict = Dictionary::new();
        dict.insert("serial".into(), Value::String("S1".into()));
        dict.insert("build".into(), Value::String("B1".into()));
        if with_generation {
            dict.insert("passcodeGeneration".into(), Value::Integer(7.into()));
        }
        dict.insert("com.apple.securebackup.timestamp".into(), Value::String("2026-01-01 00:00:00".into()));
        dict.insert("bottleID".into(), Value::String("BOTTLE".into()));
        dict.insert("ClientMetadata".into(), Value::Dictionary(Dictionary::new()));
        dict.insert("escrowedSPKI".into(), Value::Data(vec![1, 2, 3]));
        dict.insert("SecureBackupUsesMultipleiCSCs".into(), Value::Boolean(true));
        dict.insert("BackupKeybagDigest".into(), Value::Data(vec![4]));
        dict.insert("peerInfo".into(), Value::Data(vec![5]));
        Value::Dictionary(dict)
    }

    #[test]
    fn keeps_a_present_passcode_generation() {
        assert_eq!(parse_metadata_lenient(&record(true)).unwrap().passcode_generation, 7);
    }

    #[test]
    fn defaults_a_missing_passcode_generation() {
        assert_eq!(parse_metadata_lenient(&record(false)).unwrap().passcode_generation, 0);
    }

    #[test]
    fn still_rejects_other_missing_fields() {
        let mut value = record(true);
        value.as_dictionary_mut().unwrap().remove("serial");
        let err = parse_metadata_lenient(&value).unwrap_err();
        assert!(err.contains("serial"), "{err}");
    }

    #[test]
    fn rejects_non_dictionaries() {
        assert!(parse_metadata_lenient(&Value::String("x".into())).is_err());
    }

    #[test]
    fn pairs_bottles_by_label_in_apple_order() {
        let bottle = |id: &str| cloudkit_proto::EscrowData { id: Some(id.into()), ..Default::default() };
        let valid = vec![bottle("B"), bottle("A"), bottle("C")];
        let records = vec![
            ("A".to_string(), Some(record(false))),
            ("B".to_string(), Some(record(false))),
            ("C".to_string(), None),
        ];
        let paired = pair_bottles(&valid, &records);
        let ids: Vec<&str> = paired.iter().map(|(b, _)| b.id()).collect();
        assert_eq!(ids, vec!["B", "A"], "C has no readable metadata");
    }
}
