use std::{collections::HashMap, sync::Arc, time::{Duration, SystemTime}};

use keystore::software::plist_to_bin;
use log::warn;
use omnisette::AnisetteProvider;
use openssl::{conf, ec::{EcGroup, EcKey}, hash::MessageDigest, nid::Nid, pkey::{PKey, Private}, sign::Signer};
use plist::{Data, Date, Dictionary, Value};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use uuid::Uuid;
use crate::{keychain::{KeychainClientState, SavedKeychainZone, decrypt_entry}, util::{bin_deserialize, bin_serialize, date_deserialize, date_deserialize_opt, date_serialize, date_serialize_opt, date_to_ms, ec_key_from_apple, ec_key_to_apple, ms_to_date}};

use crate::{PushError, keychain::KeychainClient};

pub struct PasswordManager<P: AnisetteProvider> {
    keychain: Arc<KeychainClient<P>>,
}

#[derive(Deserialize)]
pub struct PasswordWebsiteMeta {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    pub srvr: String,
    // should be com.apple.password-manager.website-metadata
    pub agrp: String,
    #[serde(rename = "v_Data", deserialize_with = "bin_deserialize")]
    pub data: Vec<u8>,
}

#[derive(Deserialize)]
pub struct PasswordWebsiteMetaData {
    #[serde(default, deserialize_with = "date_deserialize_opt")]
    pub wn_dm: Option<u64>,
    pub wn: Option<String>,
    #[serde(default, deserialize_with = "date_deserialize_opt")]
    pub wn_dr: Option<u64>,
}

impl PasswordWebsiteMeta {
    pub fn get_meta(&self) -> Result<PasswordWebsiteMetaData, PushError> {
        Ok(plist::from_bytes(self.data.as_ref())?)
    }
}

impl PasswordEntry for PasswordWebsiteMeta {
    fn verify(&self) -> bool {
        self.agrp == "com.apple.password-manager.website-metadata"
    }

    fn make_keychain(&self) -> Dictionary {
        Dictionary::from_iter([
            ("labl".to_string(), Value::String(format!(
                "Website Metadata for {}",
                self.srvr
            ))),
            ("tomb".to_string(), Value::Integer(0.into())),
            ("acct".to_string(), Value::String(String::new())),
            ("v_Data".to_string(), Value::Data(self.data.clone())),
            ("atyp".to_string(), Value::String(String::new())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("path".to_string(), Value::String(String::new())),
            ("desc".to_string(), Value::String("Website Metadata".to_string())),
            ("musr".to_string(), Value::Data(vec![])),
            ("sdmn".to_string(), Value::String(String::new())),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("srvr".to_string(), Value::String(self.srvr.clone())),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("pdmn".to_string(), Value::String("ak".to_string())),
            ("ptcl".to_string(), Value::String("htps".to_string())),
            ("agrp".to_string(), Value::String(self.agrp.clone())),
            ("class".to_string(), Value::String("inet".to_string())),
            ("port".to_string(), Value::Integer(0.into())),
        ])
    }

    fn view() -> &'static str {
        "Passwords"
    }
}

#[derive(Deserialize)]
pub struct PasswordRawEntry {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    pub srvr: String,
    pub acct: String,
    // should be com.apple.cfnetwork
    pub agrp: String,
    #[serde(rename = "v_Data", deserialize_with = "bin_deserialize")]
    pub data: Vec<u8>,
}

impl PasswordEntry for PasswordRawEntry {
    fn verify(&self) -> bool {
        self.agrp == "com.apple.cfnetwork"
    }

    fn make_keychain(&self) -> Dictionary {
        Dictionary::from_iter([
            ("labl".to_string(), Value::String(format!(
                "{} ({})",
                self.srvr, self.acct
            ))),
            ("tomb".to_string(), Value::Integer(0.into())),
            ("acct".to_string(), Value::String(self.acct.clone())),
            ("v_Data".to_string(), Value::Data(self.data.clone())),
            ("atyp".to_string(), Value::String("form".to_string())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("path".to_string(), Value::String(String::new())),
            ("desc".to_string(), Value::String("Web form password".to_string())),
            ("musr".to_string(), Value::Data(vec![])),
            ("sdmn".to_string(), Value::String(String::new())),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("srvr".to_string(), Value::String(self.srvr.clone())),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("pdmn".to_string(), Value::String("ak".to_string())),
            ("ptcl".to_string(), Value::String("htps".to_string())),
            ("agrp".to_string(), Value::String(self.agrp.clone())),
            ("class".to_string(), Value::String("inet".to_string())),
            ("port".to_string(), Value::Integer(0.into())),
        ])
    }

    fn view() -> &'static str {
        "Passwords"
    }
}

#[derive(Deserialize, Clone)]
pub struct PasswordManagerMeta {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    pub srvr: String,
    pub acct: String,
    // should be com.apple.password-manager
    pub agrp: String,
    #[serde(rename = "v_Data", deserialize_with = "bin_deserialize")]
    pub data: Vec<u8>,
}

impl PasswordEntry for PasswordManagerMeta {
    fn verify(&self) -> bool {
        self.agrp == "com.apple.password-manager"
    }

    fn make_keychain(&self) -> Dictionary {
        Dictionary::from_iter([
            ("labl".to_string(), Value::String(format!(
                "Password Manager Metadata: {} ({})",
                self.srvr, self.acct
            ))),
            ("tomb".to_string(), Value::Integer(0.into())),
            ("acct".to_string(), Value::String(self.acct.clone())),
            ("v_Data".to_string(), Value::Data(self.data.clone())),
            ("atyp".to_string(), Value::String("form".to_string())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("path".to_string(), Value::String(String::new())),
            ("type".to_string(), Value::Integer(1_835_626_085.into())),
            ("desc".to_string(), Value::String("Password Manager Metadata".to_string())),
            ("sdmn".to_string(), Value::String(String::new())),
            ("musr".to_string(), Value::Data(vec![])),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("srvr".to_string(), Value::String(self.srvr.clone())),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("pdmn".to_string(), Value::String("ak".to_string())),
            ("ptcl".to_string(), Value::String("htps".to_string())),
            ("agrp".to_string(), Value::String(self.agrp.clone())),
            ("class".to_string(), Value::String("inet".to_string())),
            ("port".to_string(), Value::Integer(0.into())),
        ])
    }

    fn view() -> &'static str {
        "Passwords"
    }
}

#[derive(Deserialize, Serialize)]
pub struct PasswordManagerMetaChange {
    #[serde(rename = "d", deserialize_with = "date_deserialize", serialize_with = "date_serialize")]
    pub date: u64,
    #[serde(rename = "p")]
    pub password: String,
    #[serde(rename = "op")]
    pub old_password: Option<String>,
    pub id: String,
    #[serde(rename = "t")]
    pub typ: String,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PasswordManagerTotp {
    #[serde(deserialize_with = "bin_deserialize", serialize_with = "bin_serialize")]
    pub secret: Vec<u8>,
    pub digits: u32,
    pub issuer: Option<String>,
    pub period: u32,
    #[serde(rename = "_initialDate", deserialize_with = "date_deserialize", serialize_with = "date_serialize")]
    pub initial_date: u64,
    pub algorithm: u32,
    pub account_name: Option<String>,
    #[serde(rename = "originalURL")]
    pub original_url: Option<String>,
}

impl PasswordManagerTotp {
    pub fn generate_otp(&self) -> Result<(u32, u64), PushError> {
        let time_ms = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH).unwrap().as_millis() as u64 - self.initial_date;
        let counter = time_ms / 1000 / self.period as u64;

        let sig_hmac = PKey::hmac(&self.secret)?;
        let h = Signer::new(match self.algorithm {
            0 => MessageDigest::sha1(),
            1 => MessageDigest::sha256(),
            2 => MessageDigest::sha512(),
            _unk => return Err(PushError::UnknownTotpAlgorithm(_unk))
        }, &sig_hmac)?.sign_oneshot_to_vec(&counter.to_be_bytes())?;

        let offset = (h.last().unwrap() & 0x0f) as usize;

        let result = u32::from_be_bytes(h[offset..offset + 4].try_into().unwrap()) & 0x7fffffff;
        let otp = result % 10_u32.pow(self.digits);

        Ok((otp, (counter + 1) * self.period as u64 + self.initial_date / 1000))
    }
}

#[derive(Deserialize, Serialize)]
pub struct PasswordManagerAltDomain {
    #[serde(rename = "s")]
    pub domain: String,
}

#[derive(Deserialize, Serialize)]
pub struct PasswordManagerMetaData {
    #[serde(rename = "s_hi", default, skip_serializing_if="Vec::is_empty")]
    pub history: Vec<PasswordManagerMetaChange>,
    #[serde(rename = "s_as")]
    pub alt_domains: Vec<PasswordManagerAltDomain>,
    pub totp: Option<PasswordManagerTotp>,
    #[serde(default, skip_serializing_if="HashMap::is_empty")]
    pub ctxt: HashMap<String, PasswordManagerMetaDataCtx>,
}

#[derive(Deserialize, Serialize)]
pub struct PasswordManagerMetaDataCtx {
    #[serde(rename = "lUsed")]
    pub last_used: f64,
}

impl PasswordManagerMeta {
    pub fn get_password_data(&self) -> Result<PasswordManagerMetaData, PushError> {
        if let Err(e) = plist::from_bytes::<PasswordManagerMetaData>(self.data.as_ref()) {
            warn!("Err decoding password data {e} {:?}", plist::from_bytes::<Value>(self.data.as_ref()));
        }
        Ok(plist::from_bytes(self.data.as_ref())?)
    }
    pub fn get_data(data: &PasswordManagerMetaData) -> Result<Vec<u8>, PushError> {
        Ok(plist_to_bin(data)?)
    }
}

pub trait PasswordEntry: DeserializeOwned {
    fn verify(&self) -> bool;
    fn make_keychain(&self) -> Dictionary;
    fn view() -> &'static str;
    fn class() -> &'static str {
        "classA"
    }
}

#[derive(Deserialize)]
pub struct WifiPassword {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    pub acct: String,
    // should be AirPort
    pub svce: String,
    #[serde(rename = "v_Data", deserialize_with = "bin_deserialize")]
    pub data: Vec<u8>,
}

impl PasswordEntry for WifiPassword {
    fn verify(&self) -> bool {
        self.svce == "AirPort"
    }

    fn make_keychain(&self) -> Dictionary {
        Dictionary::from_iter([
            ("v_Data".to_string(), Value::Data(self.data.clone())),
            ("musr".to_string(), Value::Data(vec![])),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("agrp".to_string(), Value::String("apple".to_string())),
            ("acct".to_string(), Value::String(self.acct.clone())),
            ("labl".to_string(), Value::String(self.acct.clone())),
            ("desc".to_string(), Value::String("AirPort network password".to_string())),
            ("pdmn".to_string(), Value::String("ck".to_string())),
            ("class".to_string(), Value::String("genp".to_string())),
            ("svce".to_string(), Value::String(self.svce.clone())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("tomb".to_string(), Value::Integer(0.into())),
        ])
    }

    fn view() -> &'static str {
        "WiFi"
    }

    fn class() -> &'static str {
        "classC" // low class
    }
}

#[derive(Deserialize)]
pub struct Passkey {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    // should be com.apple.webkit.webauthn
    pub agrp: String,
    pub labl: String, // site
    #[serde(rename = "v_Data", deserialize_with = "bin_deserialize")]
    pub data: Vec<u8>, // key
    #[serde(deserialize_with = "bin_deserialize")]
    pub atag: Vec<u8>, // tag (CBOR user field)
    #[serde(deserialize_with = "bin_deserialize")]
    pub klbl: Vec<u8>, // credential ID
}

impl Passkey {
    pub fn get_key(&self) -> EcKey<Private> {
        let key_group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1).unwrap();
        ec_key_from_apple(&self.data, &key_group)
    }

    pub fn encode_key(key: EcKey<Private>) -> Vec<u8> {
        ec_key_to_apple(&key)
    }
}

impl PasswordEntry for Passkey {
    fn verify(&self) -> bool {
        self.agrp == "com.apple.webkit.webauthn"
    }

    fn make_keychain(&self) -> Dictionary {
        let apple_epoch = SystemTime::UNIX_EPOCH + Duration::from_secs(978_307_200);

        Dictionary::from_iter([
            ("class".to_string(), Value::String("keys".to_string())),
            ("asen".to_string(), Value::Integer(0.into())),
            ("priv".to_string(), Value::Integer(1.into())),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("modi".to_string(), Value::Integer(1.into())),
            ("next".to_string(), Value::Integer(0.into())),
            ("sdat".to_string(), Value::Date(Date::from(apple_epoch))),
            ("vyrc".to_string(), Value::Integer(0.into())),
            ("bsiz".to_string(), Value::Integer(256.into())),
            ("vrfy".to_string(), Value::Integer(0.into())),
            ("type".to_string(), Value::Integer(73.into())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("sens".to_string(), Value::Integer(0.into())),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("extr".to_string(), Value::Integer(1.into())),
            ("tomb".to_string(), Value::Integer(0.into())),
            ("alis".to_string(), Value::Data(self.klbl.clone())),
            ("wrap".to_string(), Value::Integer(0.into())),
            ("perm".to_string(), Value::Integer(1.into())),
            ("pdmn".to_string(), Value::String("ak".to_string())),
            ("musr".to_string(), Value::Data(vec![])),
            ("snrc".to_string(), Value::Integer(0.into())),
            ("sign".to_string(), Value::Integer(1.into())),
            ("esiz".to_string(), Value::Integer(256.into())),
            ("decr".to_string(), Value::Integer(1.into())),
            ("atag".to_string(), Value::Data(self.atag.clone())),
            ("edat".to_string(), Value::Date(Date::from(apple_epoch))),
            ("klbl".to_string(), Value::Data(self.klbl.clone())),
            ("crtr".to_string(), Value::Integer(0.into())),
            ("unwp".to_string(), Value::Integer(1.into())),
            ("v_Data".to_string(), Value::Data(self.data.clone())),
            ("encr".to_string(), Value::Integer(0.into())),
            ("kcls".to_string(), Value::Integer(1.into())),
            ("agrp".to_string(), Value::String(self.agrp.clone())),
            ("labl".to_string(), Value::String(self.labl.clone())),
            ("drve".to_string(), Value::Integer(1.into())),
        ])
    }

    fn view() -> &'static str {
        "Passwords"
    }
}

#[derive(Deserialize)]
pub struct CreditCard {
    #[serde(deserialize_with = "date_deserialize")]
    pub cdat: u64,
    #[serde(deserialize_with = "date_deserialize")]
    pub mdat: u64,
    // should be SafariCreditCardEntries
    pub svce: String,
    pub acct: String,
    #[serde(rename = "v_Data")]
    pub data: Data,
}

impl PasswordEntry for CreditCard {
    fn verify(&self) -> bool {
        self.svce == "SafariCreditCardEntries"
    }

    fn make_keychain(&self) -> Dictionary {
        Dictionary::from_iter([
            ("v_Data".to_string(), Value::Data(self.data.as_ref().to_vec())),
            ("acct".to_string(), Value::String(self.acct.clone())),
            ("tomb".to_string(), Value::Integer(0.into())),
            ("svce".to_string(), Value::String(self.svce.clone())),
            ("sha1".to_string(), Value::Data(rand::random::<[u8; 20]>().to_vec())),
            ("musr".to_string(), Value::Data(vec![])),
            ("cdat".to_string(), Value::Date(ms_to_date(self.cdat))),
            ("mdat".to_string(), Value::Date(ms_to_date(self.mdat))),
            ("pdmn".to_string(), Value::String("ak".to_string())),
            ("agrp".to_string(), Value::String("com.apple.safari.credit-cards".to_string())),
            ("class".to_string(), Value::String("genp".to_string())),
            ("labl".to_string(), Value::String("SafariCreditCardEntries".to_string())),
        ])
    }

    fn view() -> &'static str {
        "CreditCards"
    }
}

impl CreditCard {
    pub fn get_credit_card_data(&self) -> Result<CreditCardData, PushError> {
        Ok(plist::from_bytes(self.data.as_ref())?)
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreditCardDataSavePromptState {
    pub expiration: String,
    pub primary_account_number: String,
    pub security_code: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreditCardDataEligibility {
    pub card_state: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreditCardData {
    // iOS 26
    pub card_eligibility_state: Option<CreditCardDataEligibility>,
    pub displayable_last_four: Option<String>,
    pub save_prompt_state: Option<CreditCardDataSavePromptState>,
    pub identifier: Option<String>,
    #[serde(rename = "FPANHash")]
    pub fpan_hash: Option<String>,
    pub version: Option<String>,
    pub credential_type: Option<u32>,

    // macos
    #[serde(rename = "PromptToSaveSecurityCode")]
    pub prompt_to_save_security_code: Option<bool>,

    // and now come the PascalCase ones, because, Apple
    #[serde(rename = "CardholderName")]
    pub cardholder_name: String,
    #[serde(rename = "ExpirationDate", default, deserialize_with = "date_deserialize_opt", serialize_with = "date_serialize_opt")]
    pub expiration_date: Option<u64>,
    #[serde(rename = "LastUsedDate", default, deserialize_with = "date_deserialize_opt", serialize_with = "date_serialize_opt")]
    pub last_used_date: Option<u64>,
    #[serde(rename = "CardNameUIString")]
    pub card_name_ui_string: String,
    #[serde(rename = "CardSecurityCode")]
    pub card_security_code: Option<String>,
    #[serde(rename = "CardNumber")]
    pub card_number: String,
}

pub struct SiteConfig {
    pub website_meta: Option<(String, PasswordWebsiteMeta)>,
    pub passwords: HashMap<String, PasswordRawEntry>,
    pub passwords_meta: HashMap<String, PasswordManagerMeta>,
    pub passkeys: HashMap<String, Passkey>,
}


impl<P: AnisetteProvider> PasswordManager<P> {
    pub fn new(keychain: Arc<KeychainClient<P>>) -> Self {
        Self {
            keychain
        }
    }

    pub async fn sync_passwords(&self) -> Result<(), PushError> {
        self.keychain.sync_keychain(&["WiFi", "Passwords", "CreditCards"]).await
    }

    pub fn iter_password_entries<'a, T: PasswordEntry>(&self, items: &'a KeychainClientState) -> impl Iterator<Item = (String, T)> + 'a {
        let siv_key = items.get_keychain_access_key().expect("Could not get password entry");
        items.items[T::view()].keys.iter().filter_map(move |(i, k)| {
            let dict = decrypt_entry(k, &siv_key);
            let v: T = plist::from_value(&Value::Dictionary(dict)).ok()?;
            if v.verify() {
                Some((i.clone(), v))
            } else { None }
        })
    }
    
    pub async fn get_password_entries<T: PasswordEntry>(&self) -> HashMap<String, T> {
        let state = self.keychain.state.read().await;
        self.iter_password_entries::<T>(&state).collect()
    }

    pub async fn get_password_entry<T: PasswordEntry>(&self, id: &str) -> Result<T, PushError> {
        let keychain = self.keychain.state.read().await;
        let siv_key = keychain.get_keychain_access_key()?;
        let result = &keychain.items[T::view()].keys[id];
        let dict = decrypt_entry(result, &siv_key);
        Ok(plist::from_value(&Value::Dictionary(dict))?)
    }

    pub async fn get_password_for_site(&self, site: String) -> SiteConfig {
        let state = self.keychain.state.read().await;
        let config = SiteConfig {
            website_meta: self.iter_password_entries::<PasswordWebsiteMeta>(&state).find(|(_, p)| p.srvr == site),
            passwords: self.iter_password_entries::<PasswordRawEntry>(&state).filter(|(_, p)| p.srvr == site).collect(),
            passwords_meta: self.iter_password_entries::<PasswordManagerMeta>(&state).filter(|(_, p)| {
                if let Ok(details) = p.get_password_data() {
                    if details.alt_domains.iter().any(|d| d.domain == site) {
                        return true
                    }
                }
                p.srvr == site
            }).collect(),
            passkeys: self.iter_password_entries::<Passkey>(&state).filter(|(_, p)| p.labl == site).collect(),
        };
        config
    }

    pub async fn insert_password(&self, id: &str, password: &PasswordManagerMeta) -> Result<(), PushError> {
        let state = self.keychain.state.read().await;
        let item = password.get_password_data()?;
        let (password_id, mut relevant_password) = self.iter_password_entries::<PasswordRawEntry>(&state)
            .find(|(_, p)| p.srvr == password.srvr && p.acct == password.acct).unwrap_or_else(|| (Uuid::new_v4().to_string().to_uppercase(), PasswordRawEntry {
                cdat: date_to_ms(SystemTime::now().into()),
                mdat: date_to_ms(SystemTime::now().into()),
                srvr: password.srvr.clone(),
                acct: password.acct.clone(),
                agrp: "com.apple.cfnetwork".to_string(),
                data: vec![],
            }));
        drop(state);

        let raw_password = item.history.last().expect("No password!").password.clone().into_bytes();
        if relevant_password.data != raw_password {
            relevant_password.mdat = date_to_ms(SystemTime::now().into());
            relevant_password.data = raw_password;
            
            self.insert_password_entry(&password_id, &relevant_password).await?;
        }

        self.insert_password_entry(id, password).await?;
        
        Ok(())
    }

    pub async fn insert_password_entry<T: PasswordEntry>(&self, id: &str, entry: &T) -> Result<(), PushError> {
        self.keychain.insert_keychain(id, T::view(), T::class(), entry.make_keychain(), None, None).await
    }

    pub async fn delete_password(&self, id: &str) -> Result<(), PushError> {
        let password: PasswordManagerMeta = self.get_password_entry(id).await?;
        let state = self.keychain.state.read().await;
        let result = self.iter_password_entries::<PasswordRawEntry>(&state)
            .find(|(_, p)| p.srvr == password.srvr && p.acct == password.acct);
        drop(state);
        if let Some((id, _)) = result {
            self.delete_password_entry::<PasswordRawEntry>(&id).await?;
        }
        
        self.delete_password_entry::<PasswordManagerMeta>(id).await?;
        Ok(())
    }

    pub async fn delete_password_entry<T: PasswordEntry>(&self, id: &str) -> Result<(), PushError> {
        self.keychain.delete_keychain(id, T::view()).await
    }
}
