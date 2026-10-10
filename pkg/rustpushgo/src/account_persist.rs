//! Per-login rustpush account state the bridge persists across restarts
//! (R6, after corten af1fe3e1).
//!
//! Since rustpush 8c8d32b the account keeps its tokens (with real expirations)
//! in a serializable `PersistAccountData`, and the TokenProvider keeps the
//! MobileMe delegate in a serializable `TokenProviderState`. The bridge stores
//! both as one opaque blob in `UserLoginMetadata.AccountPersistBlob` and hands
//! it back to `restore_token_provider`, so a restart reuses tokens that are
//! still valid instead of forcing an SRP login.
//!
//! The blob is read from the live account/provider when Go asks for it (see
//! `WrappedTokenProvider::get_account_persist_blob`) rather than recorded from
//! rustpush's persistence callbacks: both records are public, the read is
//! always current (the bridge also edits `persisted.tokens` itself, e.g. the
//! PET snapshot merge, which fires no callback), and nothing outlives the
//! login that owns it.

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use icloud_auth::PersistAccountData;
use log::{info, warn};
use rustpush::{MobileMeDelegateResponse, TokenProviderState};

use crate::{plist_value_as_u64, WrappedError};

/// Blob layout: base64 of a binary plist `{version, account, provider?}`.
/// `account` / `provider` use rustpush's own serde layout, so a field added
/// upstream arrives without a schema change. Binary plist (not JSON) so the
/// `plist::Data` hashed password round-trips.
const PERSIST_BLOB_VERSION: u64 = 1;

fn err(msg: String) -> WrappedError {
    WrappedError::GenericError { msg }
}

/// Serialize the account record and, when known, the MobileMe delegate state.
pub(crate) fn encode_persist_blob(
    account: &PersistAccountData,
    provider: Option<&TokenProviderState>,
) -> Result<String, WrappedError> {
    let mut root = plist::Dictionary::new();
    root.insert("version".into(), plist::Value::Integer(PERSIST_BLOB_VERSION.into()));
    root.insert("account".into(), plist::to_value(account)
        .map_err(|e| err(format!("Could not serialize the account record: {}", e)))?);
    if let Some(provider) = provider {
        root.insert("provider".into(), plist::to_value(provider)
            .map_err(|e| err(format!("Could not serialize the MobileMe delegate state: {}", e)))?);
    }
    let mut bytes = Vec::new();
    plist::to_writer_binary(&mut bytes, &plist::Value::Dictionary(root))
        .map_err(|e| err(format!("Failed to serialize account persist blob: {}", e)))?;
    Ok(BASE64_STANDARD.encode(bytes))
}

pub(crate) fn decode_persist_blob(
    blob: &str,
) -> Result<(PersistAccountData, Option<TokenProviderState>), WrappedError> {
    let bytes = BASE64_STANDARD.decode(blob.trim())
        .map_err(|e| err(format!("Account persist blob is not base64: {}", e)))?;
    let root: plist::Dictionary = plist::from_bytes(&bytes)
        .map_err(|e| err(format!("Invalid account persist blob: {}", e)))?;
    let version = root.get("version").and_then(plist_value_as_u64).unwrap_or(0);
    if version != PERSIST_BLOB_VERSION {
        return Err(err(format!("Unsupported account persist blob version {}", version)));
    }
    let account = root.get("account")
        .ok_or_else(|| err("Account persist blob has no account record".into()))?;
    let account: PersistAccountData = plist::from_value(account)
        .map_err(|e| err(format!("Invalid persisted account record: {}", e)))?;
    let provider = root.get("provider")
        .map(|value| plist::from_value::<TokenProviderState>(value)
            .map_err(|e| err(format!("Invalid persisted MobileMe delegate state: {}", e))))
        .transpose()?;
    Ok((account, provider))
}

/// Choose the state to restore a login from. The blob wins when it decodes
/// and belongs to the same Apple account (adsid) as the stored SPD; anything
/// else (no blob, an unreadable one, or another account's) falls back to the
/// SPD rebuild. The SPD's hashed password fills in if the blob lacks one.
pub(crate) fn select_restore_state(
    blob: Option<&str>,
    spd_persisted: PersistAccountData,
) -> (PersistAccountData, Option<TokenProviderState>) {
    let Some(blob) = blob.filter(|b| !b.trim().is_empty()) else {
        return (spd_persisted, None);
    };
    match decode_persist_blob(blob) {
        Ok((mut account, provider)) if account.adsid == spd_persisted.adsid => {
            if account.hashed_password.is_empty() {
                account.hashed_password = spd_persisted.hashed_password;
            }
            info!("Restoring account state from the persisted rustpush blob");
            (account, provider)
        }
        Ok(_) => {
            warn!("Persisted rustpush blob belongs to a different account; rebuilding from SPD");
            (spd_persisted, None)
        }
        Err(e) => {
            warn!("Ignoring unreadable persisted rustpush blob; rebuilding from SPD: {}", e);
            (spd_persisted, None)
        }
    }
}

/// rustpush's MobileMe delegate state for a delegate fetched at `refreshed`.
/// The state's fields are private but serde-derived, so it is assembled
/// through its plist form.
pub(crate) fn provider_state_from_delegate(
    delegate: &MobileMeDelegateResponse,
    refreshed: std::time::SystemTime,
) -> Result<TokenProviderState, WrappedError> {
    let mut root = plist::Dictionary::new();
    root.insert("mme".into(), plist::to_value(delegate)
        .map_err(|e| err(format!("Could not serialize MobileMe delegate: {}", e)))?);
    root.insert("refreshed".into(), plist::to_value(&refreshed)
        .map_err(|e| err(format!("Could not serialize delegate timestamp: {}", e)))?);
    plist::from_value(&plist::Value::Dictionary(root))
        .map_err(|e| err(format!("Could not seed MobileMe delegate state: {}", e)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, SystemTime};

    fn account(adsid: &str) -> PersistAccountData {
        let mut account = PersistAccountData {
            username: "user@example.com".into(),
            first_name: Some("Jane".into()),
            hashed_password: vec![0, 1, 2, 250, 255],
            adsid: adsid.into(),
            dsid: 42,
            acname: "user@example.com".into(),
            ..Default::default()
        };
        account.tokens.insert("com.apple.gs.idms.pet".into(), icloud_auth::FetchedToken {
            token: "pet-token".into(),
            expiration: SystemTime::UNIX_EPOCH + Duration::from_secs(4_000_000_000),
        });
        account
    }

    #[test]
    fn blob_round_trips_tokens_and_binary_password() {
        let blob = encode_persist_blob(&account("A1"), Some(&TokenProviderState::default())).unwrap();
        let (decoded, provider) = decode_persist_blob(&blob).unwrap();
        assert_eq!(decoded.hashed_password, vec![0, 1, 2, 250, 255]);
        assert_eq!(decoded.adsid, "A1");
        assert_eq!(decoded.dsid, 42);
        let pet = &decoded.tokens["com.apple.gs.idms.pet"];
        assert_eq!(pet.token, "pet-token");
        assert_eq!(pet.expiration, SystemTime::UNIX_EPOCH + Duration::from_secs(4_000_000_000));
        assert!(provider.is_some());
    }

    #[test]
    fn provider_state_is_optional() {
        let blob = encode_persist_blob(&account("A1"), None).unwrap();
        let (_, provider) = decode_persist_blob(&blob).unwrap();
        assert!(provider.is_none());
    }

    #[test]
    fn matching_blob_wins_over_spd() {
        let blob = encode_persist_blob(&account("A1"), None).unwrap();
        let spd = PersistAccountData { adsid: "A1".into(), ..Default::default() };
        let (chosen, _) = select_restore_state(Some(&blob), spd);
        assert!(chosen.tokens.contains_key("com.apple.gs.idms.pet"));
    }

    #[test]
    fn other_accounts_blob_falls_back_to_spd() {
        let blob = encode_persist_blob(&account("A1"), None).unwrap();
        let spd = PersistAccountData { adsid: "B2".into(), ..Default::default() };
        let (chosen, provider) = select_restore_state(Some(&blob), spd);
        assert_eq!(chosen.adsid, "B2");
        assert!(chosen.tokens.is_empty());
        assert!(provider.is_none());
    }

    #[test]
    fn garbage_or_missing_blob_falls_back_to_spd() {
        for blob in [None, Some(""), Some("not base64!"), Some("aGVsbG8=")] {
            let spd = PersistAccountData { adsid: "A1".into(), ..Default::default() };
            let (chosen, _) = select_restore_state(blob, spd);
            assert!(chosen.tokens.is_empty(), "blob {:?} must fall back", blob);
        }
    }

    #[test]
    fn blob_without_password_takes_the_spd_one() {
        let mut stored = account("A1");
        stored.hashed_password.clear();
        let blob = encode_persist_blob(&stored, None).unwrap();
        let spd = PersistAccountData { adsid: "A1".into(), hashed_password: vec![9, 9], ..Default::default() };
        let (chosen, _) = select_restore_state(Some(&blob), spd);
        assert_eq!(chosen.hashed_password, vec![9, 9]);
    }

    #[test]
    fn unknown_version_is_rejected() {
        let mut root = plist::Dictionary::new();
        root.insert("version".into(), plist::Value::Integer(99u64.into()));
        let mut bytes = Vec::new();
        plist::to_writer_binary(&mut bytes, &plist::Value::Dictionary(root)).unwrap();
        assert!(decode_persist_blob(&BASE64_STANDARD.encode(bytes)).is_err());
    }
}
