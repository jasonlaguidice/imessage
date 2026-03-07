
use std::{borrow::Borrow, collections::BTreeSet, io::Cursor, time::SystemTime};

use aes::{cipher::consts::U12, Aes128};
use aes_gcm::{AesGcm, Nonce, Tag};
use chrono::Utc;
use cloudkit_proto::{CloudKitEncryptor, ProtectionInfo, RecordIdentifier};
use log::{info, warn};
use omnisette::AnisetteProvider;
use openssl::{bn::{BigNum, BigNumContext}, ec::{EcGroup, EcKey, EcPoint, PointConversionForm}, hash::MessageDigest, nid::Nid, pkcs5::pbkdf2_hmac, pkey::{HasPublic, PKey, Private, Public}, sha::{sha1, sha256}, sign::{Signer, Verifier}};
use plist::{Dictionary, Value};
use prost::bytes::Bytes;
use rasn::{types::{Any, GeneralizedTime, SequenceOf, SetOf}, AsnType, Decode, Encode};
use aes_gcm::KeyInit;
use aes_gcm::AeadInPlace;
use rustls::internal::msgs;
use uuid::Uuid;
use crate::{keychain::{KeychainClient, KeychainClientState, PCSMeta}, util::{CompactECKey, base64_decode, base64_encode, decode_hex, encode_hex, kdf_ctr_hmac, rfc6637_unwrap_key, rfc6637_wrap_key}, OSConfig, PushError};

pub struct PCSService<'t> {
    pub name: &'t str,
    pub view_hint: &'t str,
    pub zone: &'t str,
    pub r#type: i64,
    pub keychain_type: i32,
    pub v2: bool,
    // use zone-level record protection, as opposed to record protection on each record
    pub global_record: bool,
}

const MASTER_SERVICE: PCSService = PCSService {
    name: "MasterKey",
    view_hint: "PCS-MasterKey",
    zone: "ProtectedCloudStorage",
    r#type: 1,
    keychain_type: 65537,
    v2: false,
    global_record: true // should be unused
};

// _add_PCSAttributes see references for types
#[derive(Clone, AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub struct PCSAttribute {
    key: u32,
    value: rasn::types::OctetString,
}

// key 3
#[derive(AsnType, Encode, Decode)]
pub struct PCSManateeFlags {
    flags: u32,
}

#[derive(AsnType, Encode, Decode)]
pub struct PCSBuildAndTime {
    #[rasn(tag(explicit(context, 0)))]
    build: String,
    #[rasn(tag(explicit(context, 1)))]
    time: GeneralizedTime,
}

#[derive(Clone, AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Default, Debug)]
pub struct PCSSignature {
    keyid: rasn::types::OctetString,
    digest: u32, // 1 is sha256, 2 is sha512 (check?)
    signature: rasn::types::OctetString,
}

// signature is this struct with signature set to none
// the ID is found in ProtectedCloudStorage Keychain store.
// this is known as a "service key"
#[derive(Clone, AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Debug)]
#[rasn(tag(explicit(application, 1)))]
pub struct PCSPublicKey {
    pcsservice: i64,
    unk1: u64,
    pub_key: rasn::types::OctetString,
    #[rasn(tag(explicit(context, 0)))]
    attributes: Option<SequenceOf<PCSAttribute>>,
    #[rasn(tag(explicit(context, 1)))]
    signature: Option<PCSSignature>,
}

impl PCSPublicKey {
    pub fn data_for_signing(&self) -> Vec<u8> {
        let mut item = self.clone();
        item.signature = None;
        rasn::der::encode(&item).unwrap()
    }

    pub fn verify<T: HasPublic>(&self, key: &EcKey<T>) -> Result<bool, PushError> {
        let key = PKey::from_ec_key(key.clone())?;
        let mut verifier = Verifier::new(MessageDigest::sha256(), &key)?;
        verifier.update(&self.data_for_signing())?;
        
        Ok(verifier.verify(&self.signature.as_ref().unwrap().signature)?)
    }

    pub fn sign(&mut self, key: &CompactECKey<Private>) -> Result<(), PushError> {
        let pkey = key.get_pkey();
        let mut verifier = Signer::new(MessageDigest::sha256(), &pkey)?;
        verifier.update(&self.data_for_signing())?;
        
        self.signature = Some(PCSSignature {
            keyid: sha256(&key.compress())[..20].to_vec().into(),
            digest: 1,
            signature: verifier.sign_to_vec().unwrap().into()
        });
        Ok(())
    }
}

pub async fn get_boundary_key(service: &PCSService<'_>, keychain: &KeychainClient<impl AnisetteProvider>) -> Result<Vec<u8>, PushError> {
    let state = keychain.state.read().await;
    let existing = state.items.get(service.zone).and_then(|items| items.keys.values().find(|v|
        v.get("acct") == Some(&Value::String("PCSBoundaryKey".to_string())) && v.get("srvr") == Some(&Value::String(state.dsid.clone()))));
    if let Some(existing) = existing {
        Ok(state.get_data(existing)?.unwrap())
    } else {
        let key: [u8; 32] = rand::random();

        // create new boundary key
        let keychain_dict = Dictionary::from_iter([
            ("class", Value::String("inet".to_string())),
            ("tomb", Value::Integer(0.into())),
            ("acct", Value::String("PCSBoundaryKey".to_string())),
            ("v_Data", Value::Data(key.to_vec())),
            ("atyp", Value::Data(vec![])),
            ("sha1", Value::Data(rand::random::<[u8; 20]>().to_vec())), // don't ask, don't check lmao
            ("path", Value::String("".to_string())),
            ("musr", Value::Data(vec![])),
            ("sdmn", Value::String(base64_encode(&sha256(&key)))), // security domain
            ("cdat", Value::Date(SystemTime::now().into())),
            ("srvr", Value::String(state.dsid.to_string())),
            ("mdat", Value::Date(SystemTime::now().into())),
            ("pdmn", Value::String("ck".to_string())),
            ("ptcl", Value::Integer(0.into())),
            ("agrp", Value::String("com.apple.ProtectedCloudStorage".to_string())),
            ("vwht", Value::String(service.view_hint.to_string())),
            ("port", Value::Integer(0.into())),
        ]);

        drop(state);
        
        keychain.insert_keychain(&Uuid::new_v4().to_string().to_uppercase(), service.zone, "classC", keychain_dict, None, None).await?;

        Ok(key.to_vec())
    }
}

#[derive(Clone, AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Debug)]
#[rasn(choice)]
pub enum PCSPrivateKey {
    V1 {
        key: rasn::types::OctetString,
        public: Option<PCSPublicKey>,
    },
    #[rasn(tag(application, 5))]
    V2 {
        data: rasn::types::OctetString,
    }
}

impl PCSPrivateKey {
    pub fn new(signature_key: Option<&PCSPrivateKey>, service: i64, v2: bool, attributes: Vec<PCSAttribute>) -> Result<Self, PushError> {
        let key = CompactECKey::new()?;
        let signing_key = CompactECKey::new()?;

        let mut public = PCSPublicKey {
            pcsservice: service, 
            unk1: 1, 
            pub_key: key.compress().to_vec().into(),
            attributes: if attributes.is_empty() { None } else { Some(attributes) }, 
            signature: None
        };

        let signature_key = if let Some(signature_key) = &signature_key {
            signature_key.signing_key()
        } else {
            signing_key.clone()
        };

        public.sign(&signature_key)?;

        use prost::Message;

        Ok(if v2 {
            Self::V2 { 
                data: cloudkit_proto::ProtoPcsKey {
                    encryption_key: cloudkit_proto::ProtoPcsPrivateKey {
                        key: key.compress_private().to_vec(),
                        public: Some(rasn::der::encode(&public).unwrap()),
                    },
                    signing_key: Some(cloudkit_proto::ProtoPcsPrivateKey {
                        key: signature_key.compress_private().to_vec(),
                        public: None,
                    }),
                }.encode_to_vec().into(),
            }
        } else {
            Self::V1 {
                key: key.compress_private().to_vec().into(),
                public: Some(public)
            }
        })
    }

    // does not sync keys, make sure to sync beforehand
    pub async fn get_master_key(keychain: &KeychainClient<impl AnisetteProvider>) -> Result<Self, PushError> {
        let state = keychain.state.read().await;
        if let Some(existing) = &state.items[MASTER_SERVICE.zone].get_current_key(&format!("com.apple.ProtectedCloudStorage-{}", MASTER_SERVICE.name)) {
            Ok(Self::from_dict(&existing, &state))
        } else {
            drop(state);
            let master_key = PCSPrivateKey::new_master_key()?;
            info!("Creating new master key {}", encode_hex(&master_key.key().compress()));
            master_key.save_key(&Uuid::new_v4().to_string().to_uppercase(), &keychain, &MASTER_SERVICE).await?;
            info!("Created new master key");
            Ok(master_key)
        }
    }

    // use a service struct
    pub async fn get_service_key(keychain: &KeychainClient<impl AnisetteProvider>, service: &PCSService<'_>, config: &dyn OSConfig) -> Result<Self, PushError> {
        let state = keychain.state.read().await;
        if let Some(existing) = state.items[service.zone].get_current_key(&format!("com.apple.ProtectedCloudStorage-{}", service.name)) {
            Ok(PCSPrivateKey::from_dict(existing, &state))
        } else {
            drop(state);
            let master_key = Self::get_master_key(keychain).await?;

            let service_key = PCSPrivateKey::new_service_key(&master_key, service.r#type, service.v2, config)?;
            info!("Creating new service key {} for {}", encode_hex(&master_key.key().compress()), service.name);
            service_key.save_key(&Uuid::new_v4().to_string().to_uppercase(), &keychain, service).await?;
            info!("Created new service key");
            Ok(service_key)
        }
    }

    pub fn new_service_key(master_key: &PCSPrivateKey, service: i64, v2: bool, config: &dyn OSConfig) -> Result<Self, PushError> {
        // one day i will fix the config mess, i swear...
        let data = config.get_register_meta();
        let meta = format!("{};{}", data.os_version.split_once(",").unwrap().0, data.software_version);

        let attributes = vec![
            PCSAttribute {
                key: 3,
                value: rasn::der::encode(&PCSManateeFlags {
                    flags: 0,
                }).unwrap().into(),
            },
            PCSAttribute {
                key: 1,
                value: rasn::der::encode(&PCSBuildAndTime {
                    build: meta,
                    time: Utc::now().into(),
                }).unwrap().into(),
            }
        ];
        Self::new(Some(master_key), service, v2, attributes)
    }

    pub fn new_master_key() -> Result<Self, PushError> {
        Self::new(None, 1, false, vec![])
    }

    pub fn public(&self) -> Result<PCSPublicKey, PushError> {
        use prost::Message;
        Ok(match self {
            Self::V1 { key: _, public } => public.clone().expect("no public key!"),
            Self::V2 { data } => {
                let decoded = cloudkit_proto::ProtoPcsKey::decode(Cursor::new(data))?;
                rasn::der::decode(decoded.encryption_key.public.as_ref().expect("no public key!")).unwrap()
            }
        })
    }

    pub async fn save_key(&self, uuid: &str, keychain: &KeychainClient<impl AnisetteProvider>, service: &PCSService<'_>) -> Result<(), PushError> {
        let dsid = keychain.state.read().await.dsid.clone();
        let public = self.public()?;
        if service.r#type != public.pcsservice {
            panic!("mismatched service type!")
        }
        let id = sha256(&public.pub_key);
        let keychain_dict = Dictionary::from_iter([
            ("invi", Value::Integer(1.into())), // invisible
            ("sdmn", Value::String("ProtectedCloudStorage".to_string())), // security domain
            ("class", Value::String("inet".to_string())),
            ("srvr", Value::String(dsid.to_string())),
            ("path", Value::String("".to_string())),
            ("labl", Value::String(format!("PCS {} - {}", service.name, base64_encode(&public.pub_key[..6])))),
            ("agrp", Value::String("com.apple.ProtectedCloudStorage".to_string())),
            ("pdmn", Value::String("ck".to_string())),
            ("type", Value::Integer(service.keychain_type.into())),
            ("atyp", Value::Data(id[..20].to_vec())),
            ("port", Value::Integer(0.into())),
            ("vwht", Value::String(service.view_hint.to_string())),
            ("sha1", Value::Data(rand::random::<[u8; 20]>().to_vec())), // don't ask, don't check lmao
            ("musr", Value::Data(vec![])),
            ("cdat", Value::Date(SystemTime::now().into())),
            ("mdat", Value::Date(SystemTime::now().into())),
            ("ptcl", Value::Integer(0.into())),
            ("tomb", Value::Integer(0.into())),
            ("v_Data", Value::Data(rasn::der::encode(self).unwrap())),
            ("acct", Value::String(base64_encode(&public.pub_key))),
        ]);
        
        keychain.insert_keychain(uuid, service.zone, "classC", keychain_dict, Some(&PCSMeta {
            pcsservice: public.pcsservice,
            pcspublickey: public.pub_key.to_vec(),
            pcspublicidentity: rasn::der::encode(&public).unwrap(),
        }), Some(&format!("com.apple.ProtectedCloudStorage-{}", service.name))).await?;

        Ok(())
    }

    pub fn from_dict(dict: &Dictionary, keychain: &KeychainClientState) -> Self {
        let key = keychain.get_data(dict).expect("Failed to get data").expect("No dataa");

        let decoded: PCSPrivateKey = rasn::der::decode(&key).expect("Failed to decode private key!");

        match decoded.verify_with_keychain(keychain, dict.get("atyp").expect("No dat?").as_data().expect("Not data")) {
            Ok(true) => {},
            Ok(false) => {
                panic!("PCS Master key verification failed!");
            }
            Err(e) => {
                warn!("PCS master key verification failed {e}");
            }
        }

        decoded
    }

    pub fn key(&self) -> CompactECKey<Private> {
        use prost::Message;
        let key = match self {
            Self::V1 { key, public: _ } => key.to_vec(),
            Self::V2 { data } => {
                let decoded = cloudkit_proto::ProtoPcsKey::decode(Cursor::new(data)).unwrap();
                decoded.encryption_key.key
            }
        };
        CompactECKey::decompress_private(key[..].try_into().unwrap())
    }

    pub fn signing_key(&self) -> CompactECKey<Private> {
        use prost::Message;
        let key = match self {
            Self::V1 { key, public: _ } => key.to_vec(),
            Self::V2 { data } => {
                let decoded = cloudkit_proto::ProtoPcsKey::decode(Cursor::new(data)).unwrap();
                decoded.signing_key.unwrap_or(decoded.encryption_key).key
            }
        };
        CompactECKey::decompress_private(key[..].try_into().unwrap())
    }

    pub fn verify_with_keychain(&self, keychain: &KeychainClientState, keyid: &[u8]) -> Result<bool, PushError> {
        let public = self.public()?;
        let signature = public.signature.as_ref().expect("No signature!");
        
        if keyid == &signature.keyid[..] {
            // self signed
            public.verify(&self.signing_key())
        } else {
            let account = Value::Data(signature.keyid.to_vec());
            let item = keychain.items["ProtectedCloudStorage"].keys.values().find(|x| x.get("atyp") == Some(&account))
                .ok_or(PushError::MasterKeyNotFound)?;
            let key = keychain.get_data(item).expect("Failed to get data").expect("No dataa");

            let decoded: PCSPrivateKey = rasn::der::decode(&key).unwrap();

            if !decoded.verify_with_keychain(keychain, &signature.keyid)? {
                panic!("Parent key not valid!")
            }
            
            let key = decoded.signing_key();
            
            public.verify(&key)
        }
    }
}

fn get_ciphertext_key(ciphertext: &[u8]) -> (Vec<u8>, usize) {
    let encryption_version = ciphertext[0];
    if encryption_version != 3 {
        panic!("Unimplemented encryption version {encryption_version}");
    }

    let second_keyid_part_len = ciphertext[3] as usize;
    let total_tag = [
        &ciphertext[1..3],
        &ciphertext[4..4 + second_keyid_part_len]
    ].concat();

    (total_tag, 4 + second_keyid_part_len)
}

#[derive(AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub struct PCSKeyRef {
    pub keytype: u32,
    pub pub_key: rasn::types::OctetString,
}

#[derive(AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub struct PCSShareKey {
    decryption_key: PCSKeyRef,
    ciphertext: rasn::types::OctetString,
    flags: Option<u32>,
}

#[derive(AsnType, Encode, Decode, Debug)]
pub struct PCSKeySet {
    unk1: u32, // 0
    keyset: SetOf<PCSShareKey>,
    #[rasn(tag(explicit(context, 0)))]
    attributes: Option<SequenceOf<PCSAttribute>>,
}

#[derive(Clone)]
pub struct PCSKey(Vec<u8>);
impl PCSKey {
    fn new(eckey: &CompactECKey<Private>, wrapped: &[u8]) -> Result<Self, PushError> {
        Ok(Self(rfc6637_unwrap_key(eckey, &wrapped, "fingerprint".as_bytes())?))
    }

    fn wrap<T: HasPublic>(&self, key: &CompactECKey<T>) -> Result<Vec<u8>, PushError> {
        rfc6637_wrap_key(key, &self.0, "fingerprint".as_bytes())
    }

    pub fn random() -> Self {
        Self(rand::random::<[u8; 16]>().to_vec())
    }

    // AKA object key
    fn master_ec_key(&self) -> Result<EcKey<Private>, PushError> {
        let mut ctx = BigNumContext::new().unwrap();
        let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
        let mut output = [0u8; 128];
        pbkdf2_hmac(&self.0, "full master key".as_bytes(), 10, MessageDigest::sha256(), &mut output)?;

        // we need big endian for OpenSSL, yes the output is used as little endian
        output.reverse();

        let mut order = BigNum::new()?;
        group.order(&mut order, &mut ctx)?;

        let mut num = BigNum::from_slice(&output)?;
        num.mask_bits(order.num_bits())?;
        
        let num = if num > order {
            let mut out = BigNum::new()?;
            out.checked_sub(&num, &order)?;
            out
        } else { num };

        let mut pub_point = EcPoint::new(&group)?;
        pub_point.mul_generator(&group, &num, &ctx)?;
        Ok(EcKey::from_private_components(&group, &num, &pub_point)?)
    }

    pub fn get_share_key(&self, is_share: bool) -> Self {
        if is_share {
            Self(kdf_ctr_hmac(&self.0, "MsaeEooevaX fooo 012".as_bytes(), &[], self.0.len()))
        } else {
            self.clone()
        }
    }

    fn hmac_sign(&self, data: &[u8]) -> Result<Vec<u8>, PushError> {
        let hmackey = kdf_ctr_hmac(&self.0, "hmackey-of-masterkey".as_bytes(), &[], self.0.len());
        let hmac = PKey::hmac(&hmackey)?;
        Ok(Signer::new(MessageDigest::sha256(), &hmac)?.sign_oneshot_to_vec(&data)?)
    }

    pub fn key_id(&self) -> Result<Vec<u8>, PushError> {
        let label_key = kdf_ctr_hmac(&self.0, "master key id labell".as_bytes(), &[], self.0.len());
        let hmac = PKey::hmac(&label_key)?;
        Ok(Signer::new(MessageDigest::sha256(), &hmac)?.sign_oneshot_to_vec("M key input data 2 u".as_bytes())?)
    }

    fn decrypt(&self, ciphertext: &[u8], aad: &[u8]) -> Result<Vec<u8>, PushError> {
        let encryption_key = kdf_ctr_hmac(&self.0, "encryption key key m".as_bytes(), &[], self.0.len());

        let (required_key, header_len) = get_ciphertext_key(ciphertext);

        if &required_key[..] != &self.key_id()?[..required_key.len()] {
            panic!("Mismatched key id! Data {} key {}", encode_hex(&ciphertext[..16]), encode_hex(&self.key_id()?));
        }

        let tag_len = 12;

        let iv = &ciphertext[header_len..header_len + 12];
        let firstaad = &ciphertext[0..header_len];
        let gcm = AesGcm::<Aes128, U12, U12>::new(encryption_key[..].try_into().expect("Bad key size!"));
        let tag = &ciphertext[header_len + 12..header_len + 12 + tag_len];

        let mut text = ciphertext[header_len + 12 + tag_len..].to_vec();

        gcm.decrypt_in_place_detached(Nonce::from_slice(iv), &[firstaad, aad].concat(), &mut text, Tag::from_slice(tag)).expect("GCM error?");
        Ok(text)
    }

    fn encrypt(&self, plaintext: &[u8], aad: &[u8]) -> Result<Vec<u8>, PushError> {
        let encryption_key = kdf_ctr_hmac(&self.0, "encryption key key m".as_bytes(), &[], self.0.len());

        let gcm = AesGcm::<Aes128, U12, U12>::new(encryption_key[..].try_into().expect("Bad key size!"));

        let key_id = self.key_id()?;
        let header = [
            &[0x03u8][..],
            &key_id[0..2],
            &[0x02],
            &key_id[2..4],
        ].concat();

        let iv: [u8; 12] = rand::random();

        let mut enc_buffer = plaintext.to_vec();
        let tag = gcm.encrypt_in_place_detached(&iv.try_into().unwrap(), &[&header, aad].concat(), &mut enc_buffer).expect("encryption failed");

        let result = [
            &header[..],
            &iv,
            &tag,
            &enc_buffer,
        ].concat();
    
        Ok(result)
    }
}

pub struct PCSEncryptor {
    pub keys: Vec<PCSKey>,
    pub record_id: RecordIdentifier,
}
impl CloudKitEncryptor for PCSEncryptor {
    fn decrypt_data(&self, dec: &[u8], field_name: &str) -> Vec<u8> {
        let (required_key, _) = get_ciphertext_key(dec);

        let tag = format!("{}-{}-{}", self.record_id.zone_identifier.as_ref().unwrap().value.as_ref().unwrap().name(), self.record_id.value.as_ref().unwrap().name(), field_name);

        let key = self.keys.iter().find(|k| &k.key_id().unwrap()[..required_key.len()] == &required_key[..]).expect("required key not found!");
        key.decrypt(dec, tag.as_bytes()).expect("Decryption failed")
    }

    fn encrypt_data(&self, enc: &[u8], field_name: &str) -> Vec<u8> {
        let tag = format!("{}-{}-{}", self.record_id.zone_identifier.as_ref().unwrap().value.as_ref().unwrap().name(), self.record_id.value.as_ref().unwrap().name(), field_name);

        self.keys.first().expect("PCS keyset empty?").encrypt(enc, tag.as_bytes()).expect("Encryption failed")
    }
}

#[derive(AsnType, Encode, Decode, Debug, Default)]
pub struct PCSShareProtectionSignatureData {
    // 5 is the version. non-exist is 1, 5 is 2, 4 is 3,
    // classic is 2
    // share is 3
    version: u32,
    data: rasn::types::OctetString,
}

#[derive(AsnType, Encode, Decode, Debug)]
#[rasn(tag(explicit(application, 1)))]
pub struct PCSShareProtection {
    keyset: PCSKeySet,
    #[rasn(tag(explicit(context, 0)))]
    meta: rasn::types::OctetString, // encrypted
    #[rasn(tag(explicit(context, 1)))]
    signature_data: PCSShareProtectionSignatureData, // not sure this should be a sequence, maybe tag should be explicit, not sure
    hmac: rasn::types::OctetString,
    #[rasn(tag(explicit(context, 2)))]
    truncated_key_id: rasn::types::OctetString,
    #[rasn(tag(explicit(context, 3)))]
    signature: Option<PCSSignature>,
    #[rasn(tag(explicit(context, 4)))]
    attributes: Option<SequenceOf<PCSAttribute>>,
}

#[derive(AsnType, Encode, Decode, Default)]
pub struct PCSShareProtectionIdentitiesTag1 {
    unk1: u32,
    unk2: rasn::types::OctetString,
}

#[derive(AsnType, Encode, Decode, PartialEq, Eq, PartialOrd, Ord)]
pub struct PCSShareProtectionIdentityData {
    unk1: u32,
    keyset: rasn::types::OctetString,
}

#[derive(AsnType, Encode, Decode)]
#[rasn(tag(explicit(application, 2)))]
pub struct PCSShareProtectionKeySet {
    unk1: String,
    keys: SetOf<PCSPrivateKey>,
    unk2: SetOf<Any>,
    hash: Option<rasn::types::OctetString>,
}

impl PCSShareProtectionKeySet {
    fn make_checksum(&mut self) {
        self.hash = Some(sha256(&rasn::der::encode(self).unwrap()).to_vec().into());
    }

    fn check_checksum(&mut self) {
        let checksum = self.hash.take().unwrap();
        let checked = sha256(&rasn::der::encode(self).unwrap());

        if &checked[..] != &checksum[..] {
            panic!("Bad checksum!")
        }
        self.hash = Some(checksum);
    }
}

struct PCSDigestData(Vec<u8>);

impl PCSDigestData {
    fn verify(&self, key: &EcKey<impl HasPublic>, sig: &PCSSignature) -> Result<(), PushError> {
        let pkey = PKey::from_ec_key(key.clone())?;
        if !sig.keyid.is_empty() && &*sig.keyid != TryInto::<CompactECKey<_>>::try_into(key.clone())?.compress() {
            panic!("Mismatched key ID, expected {} got {}", encode_hex(&sig.keyid), encode_hex(&key.public_key_to_der()?))
        }

        let mut verifier = Verifier::new(MessageDigest::sha256(), &pkey)?;
        verifier.update(&self.0)?;
        if !verifier.verify(&sig.signature)? {
            return Err(PushError::VerificationFailed)
        }
        
        Ok(())
    }

    fn sign(&self, key: &EcKey<Private>, is_self: bool) -> Result<PCSSignature, PushError> {
        let pkey = PKey::from_ec_key(key.clone())?;
        let mut signer = Signer::new(MessageDigest::sha256(), &pkey)?;
        signer.update(&self.0)?;

        Ok(PCSSignature {
            keyid: if is_self { Default::default() } else { TryInto::<CompactECKey<_>>::try_into(key.clone())?.compress().to_vec().into() },
            digest: 1,
            signature: signer.sign_to_vec()?.into(),
        })
    }
}

pub struct ParticipantMeta {
    pub share_key: CompactECKey<Public>,
    pub sign_with_private_key: Option<PCSPrivateKey>,
}

#[derive(AsnType, Encode, Decode)]
pub struct PCSShareProtectionIdentities {
    #[rasn(tag(explicit(context, 0)))]
    symm_keys: Option<SetOf<rasn::types::OctetString>>,
    #[rasn(tag(explicit(context, 1)))]
    tag1: PCSShareProtectionIdentitiesTag1,
    #[rasn(tag(explicit(context, 2)))]
    identities: Option<SetOf<PCSShareProtectionIdentityData>>,
}

impl PCSShareProtection {
    fn signature_data(&self) -> PCSObjectSignature {
        rasn::der::decode(&self.signature_data.data).expect("failed to decode signature data")
    }

    fn digest_data(&self, objsig: &PCSObjectSignature) -> PCSDigestData {
        let mut data = [
            &rasn::der::encode(&self.keyset).unwrap(),
            &self.meta[..],
            &objsig.outer_sign_key_type.to_be_bytes(),
            &objsig.roll_count.to_be_bytes(),
            &objsig.symm_key_count.unwrap_or(0).to_be_bytes(),
            &objsig.public.keytype.to_be_bytes(),
            &objsig.public.pub_key[..],
        ].concat();
        if let Some(attributes) = &objsig.attributes {
            data.extend_from_slice(&rasn::der::encode(attributes).unwrap());
        }
        if let Some(ec_key_list) = &objsig.ec_key_list {
            data.extend_from_slice(&rasn::der::encode(ec_key_list).unwrap());
        }
        PCSDigestData(data)
    }

    fn hmac_data(&self) -> Vec<u8> {
        [
            &rasn::der::encode(&self.keyset).unwrap(),
            &self.meta[..],
            &rasn::der::encode(&self.signature_data()).unwrap(),
        ].concat()
    }

    pub fn decode_key_public(&self) -> Result<Vec<u8>, PushError> {
        Ok(self.keyset.keyset.first().expect("No public keyset! (bad decoding?)").decryption_key.pub_key.to_vec())
    }

    pub fn get_private_key(&self, keychain: &KeychainClientState, service: &PCSService<'_>) -> Result<PCSPrivateKey, PushError> {
        let keys = self.keyset.keyset.iter().map(|k| Value::String(base64_encode(&k.decryption_key.pub_key))).collect::<Vec<_>>();
        
        let item = keychain.items[service.zone].keys.values().find(|x| matches!(x.get("acct"), Some(x) if keys.contains(x)))
            .ok_or(PushError::ShareKeyNotFound(encode_hex(&self.decode_key_public()?)))?;
        Ok(PCSPrivateKey::from_dict(item, keychain))
    }

    pub fn decrypt_with_keychain(&self, keychain: &KeychainClientState, service: &PCSService<'_>, custom_signing: bool) -> Result<(Vec<PCSKey>, Vec<CompactECKey<Private>>), PushError> {
        let decoded = self.get_private_key(keychain, service)?;
        
        let key = decoded.key();
        info!("Decoding with {}", base64_encode(&key.compress()));
        
        let signing = decoded.signing_key();
        self.decode(&[&key], if custom_signing { Some(&signing) } else { None })
    }

    pub fn to_protection_info(&self, tag: bool) -> Result<ProtectionInfo, PushError> {
        let encoded = rasn::der::encode(self).expect("Failed to encode protection info!");
        Ok(ProtectionInfo {
            protection_info_tag: if tag { Some(encode_hex(&sha1(&encoded)).to_uppercase()) } else { None },
            protection_info: Some(encoded),
        })
    }

    pub fn get_inner_keys(&self) -> Vec<CompactECKey<Public>> {
        self.signature_data().ec_key_list.unwrap_or_default().into_iter().map(|key| 
                CompactECKey::decompress(key.pub_key.to_vec().try_into().expect("bad key lsne!"))).collect()
    }

    pub fn get_roll_count(&self) -> u32 {
        self.signature_data().roll_count
    }

    pub fn from_protection_info(info: &ProtectionInfo) -> Self {
        rasn::der::decode(info.protection_info()).expect("Bad invite protection?")
    }

    pub fn create_new(me: &CompactECKey<Private>, keys: &[CompactECKey<Private>], access: &[CompactECKey<impl HasPublic>], is_share: bool) -> Result<Self, PushError> {
        Ok(Self::create(me, keys, access, PCSKey::random(), Some(me), &[], None, 1, None, is_share)?)
    }

    pub fn get_key_attribute(&self, attr: u32) -> Option<Bytes> {
        self.keyset.attributes.clone().unwrap_or_default().into_iter().find(|i| i.key == attr).map(|i| i.value)
    }

    pub fn get_encryption_keys(&self) -> Vec<Vec<u8>> {
        self.keyset.keyset.iter().map(|k| k.decryption_key.pub_key.to_vec()).collect()
    }

    pub fn create_participant(key: &CompactECKey<impl HasPublic>, participant_key: &[CompactECKey<Private>], participant_meta: &ParticipantMeta) -> Result<Self, PushError> {
        Self::create(
            &key, 
            participant_key, 
            &[] as &[CompactECKey<Private>], 
            PCSKey::random(),
            None, 
            &[], 
            None, 
            1,
            Some(participant_meta), 
            true
        )
    }

    // don't put "me" in access
    pub fn create(
        me: &CompactECKey<impl HasPublic>, 
        keys: &[CompactECKey<Private>], 
        access: &[CompactECKey<impl HasPublic>], 
        rm_master_key: PCSKey,
        sign_with_key: Option<&EcKey<Private>>,
        extra_keys: &[PCSKey], 
        last_key: Option<PCSKey>,
        roll_count: u32,
        participant_meta: Option<&ParticipantMeta>,
        is_share: bool,
    ) -> Result<Self, PushError> {
        let master_key = rm_master_key.get_share_key(is_share);
        
        let mut keyset = PCSShareProtectionKeySet {
            unk1: "".to_string(),
            keys: BTreeSet::from_iter(keys.iter().map(|k| PCSPrivateKey::V1 {
                key: k.compress_private().to_vec().into(),
                public: None,
            })),
            unk2: BTreeSet::new(),
            hash: None,
        };
        keyset.make_checksum();

        let identities = PCSShareProtectionIdentities {
            symm_keys: if extra_keys.is_empty() { None } else { Some(extra_keys.iter().map(|i| i.0.clone().into()).collect()) },
            tag1: Default::default(),
            identities: if keys.is_empty() { None } else { Some(BTreeSet::from_iter([
                PCSShareProtectionIdentityData {
                    unk1: 0,
                    keyset: rasn::der::encode(&keyset).unwrap().into(),
                }
            ])) }
        };

        let encrypted = master_key.encrypt(&rasn::der::encode(&identities).unwrap(), &[])?;

        let mut attributes = vec![];
        if let Some(participant) = participant_meta {
            let pub_key = participant.sign_with_private_key.as_ref()
                .map(|i| i.signing_key().compress())
                .unwrap_or(me.compress());
            attributes.push(PCSAttribute {
                key: 8,
                value: rasn::der::encode(&PCSKeyRef {
                    keytype: 3,
                    pub_key: pub_key.to_vec().into(),
                }).unwrap().into(),
            });
            attributes.push(PCSAttribute {
                key: 9,
                value: rasn::der::encode(&PCSKeyRef {
                    keytype: 3,
                    pub_key: participant.share_key.compress().to_vec().into(),
                }).unwrap().into(),
            });
        }

        let mut protection = PCSShareProtection {
            keyset: PCSKeySet {
                unk1: 0,
                keyset: BTreeSet::from_iter(std::iter::once(PCSShareKey {
                    decryption_key: PCSKeyRef {
                        keytype: 3,
                        pub_key: me.compress().to_vec().into(),
                    },
                    ciphertext: rm_master_key.wrap(me)?.into(),
                    flags: None,
                }).chain(access.iter().map(|k| PCSShareKey {
                    decryption_key: PCSKeyRef {
                        keytype: 3,
                        pub_key: k.compress().to_vec().into(),
                    },
                    ciphertext: master_key.wrap(k).expect("Failed to wrap key?").into(),
                    flags: if is_share { Some(1) /* mark as readonly, marks as providing derived master key not rm master key */ } else { None },
                }))),
                attributes: if attributes.is_empty() { None } else { Some(attributes) },
            },
            meta: encrypted.into(),
            signature_data: Default::default(),
            hmac: Default::default(),
            truncated_key_id: master_key.key_id()?[..4].to_vec().into(),
            signature: Default::default(),
            attributes: None,
        };

        let mut num_ctx = BigNumContext::new()?;
        let master_ec_key = rm_master_key.master_ec_key()?;

        let mut signature_attributes = vec![];
        if !extra_keys.is_empty() {
            // add list of key ids
            signature_attributes.push(PCSAttribute { 
                key: 5, 
                value: rasn::der::encode(&extra_keys.iter().map(|key| 
                        key.key_id().expect("Bad key id??")[..4].to_vec().into()).collect::<Vec<Bytes>>()).unwrap().into(),
            });
        }

        let mut signature = PCSObjectSignature {
            roll_count,
            outer_sign_key_type: if sign_with_key.is_some() { 3 } else { 0 },
            public: PCSKeyRef {
                keytype: 1,
                pub_key: master_ec_key.public_key().to_bytes(master_ec_key.group(), PointConversionForm::UNCOMPRESSED, &mut num_ctx)?.into(),
            },
            signature: Default::default(),
            ec_key_list: if keys.is_empty() { None } else { Some(keys.iter().map(|k| PCSKeyRef {
                keytype: 3,
                pub_key: k.compress().to_vec().into(),
            }).collect()) },
            symm_key_count: if extra_keys.is_empty() { None } else { Some(extra_keys.len() as u32) },
            signature_2: None,
            attributes: if signature_attributes.is_empty() { None } else { Some(signature_attributes) },
        };

        let digest_data = protection.digest_data(&signature);
        signature.signature = digest_data.sign(&master_ec_key, true)?;

        if let Some(last_key) = last_key {
            let my_sig = digest_data.sign(&last_key.master_ec_key()?, true)?;
            signature.signature_2 = Some(my_sig);
        }

        protection.signature_data = PCSShareProtectionSignatureData {
            version: if is_share { 4 } else { 5 },
            data: rasn::der::encode(&signature).unwrap().into(),
        };

        let mut attributes = vec![];
        
        if let Some(share) = participant_meta.and_then(|i| i.sign_with_private_key.as_ref()) {
            let signature = digest_data.sign(&share.signing_key(), false)?;

            attributes.push(PCSAttribute {
                key: 7,
                value: rasn::der::encode(&signature).unwrap().into(),
            });
        }
        
        if let Some(sign_with_key) = sign_with_key {
            let signature = digest_data.sign(&sign_with_key, false)?;
            protection.signature = Some(signature);
        }

        if !attributes.is_empty() {
            protection.attributes = Some(attributes);
        }

        protection.hmac = master_key.hmac_sign(&protection.hmac_data())?.into();

        Ok(protection)
    }

    pub fn get_signer(&self) -> Option<CompactECKey<Public>> {
        self.signature.as_ref().map(|a| CompactECKey::decompress(a.keyid.to_vec().try_into().expect("Key ID")))
    }

    pub fn decode(&self, keys: &[impl Borrow<CompactECKey<Private>>], custom_signing_key: Option<&CompactECKey<impl HasPublic>>) -> Result<(Vec<PCSKey>, Vec<CompactECKey<Private>>), PushError> {
        info!("Decoding share protection!");
        let (key, share_key) = keys.iter().find_map(|key| {
            let search_ref = key.borrow().compress();
            let other = self.keyset.keyset.iter().find(|key| &*key.decryption_key.pub_key == &search_ref[..]);
            other.map(|other| (key.borrow(), other))
        }).expect("Could not find decode key!!");
        let rm_master_key = PCSKey::new(key, &share_key.ciphertext)?;
        info!("MAster key {}", encode_hex(&rm_master_key.0));

        let share_flags = share_key.flags.unwrap_or_default();
        let readonly = (share_flags & 1) != 0;

        let sig = self.signature_data();
        
        let digest_data = self.digest_data(&sig);

        info!("showed me off");

        // custom_signing_key is kind of reused here,
        // signature is not set when [4] (owner sign attr 7) is set. But sometimes we need a custom sig here. 
        if let Some(sig) = &self.signature {
            if let Some(mine) = custom_signing_key {
                digest_data.verify(mine, sig)?;
            } else {
                digest_data.verify(key, sig)?;
            }
        }

        if !readonly {
            let key = &rm_master_key.master_ec_key()?;
            match digest_data.verify(key, &sig.signature) {
                Err(PushError::VerificationFailed) => {
                    info!("First verification failed, using backup!");
                    if let Some(past_signature) = &sig.signature_2 {
                        digest_data.verify(key, &past_signature)?;
                    } else {
                        panic!("self sig check failed")
                    }
                },
                _e => _e?,
            }
        }

        info!("come");

        let owner_sign = self.attributes.clone().unwrap_or_default().into_iter().find(|a| a.key == 7);
        if let (Some(signing), Some(review)) = (custom_signing_key, owner_sign) {
            let parsed: PCSSignature = rasn::der::decode(&review.value).expect("Bad signature???a");
            digest_data.verify(signing, &parsed)?;
        }

        let mut master_key = rm_master_key.clone();
        if self.signature_data.version != 5 && !readonly {
            master_key = rm_master_key.get_share_key(true);
        }

        assert_eq!(&master_key.key_id()?[..4].to_vec(), self.truncated_key_id.as_ref());

        if &master_key.hmac_sign(&self.hmac_data())? != &self.hmac {
            panic!("HMAC check failed");
        }

        let decrypted = master_key.decrypt(&self.meta, &[])?;

        info!("here");

        let identities: PCSShareProtectionIdentities = rasn::der::decode(&decrypted).unwrap();

        let mut keys = vec![];
        for identity in identities.identities.as_ref().unwrap_or(&SetOf::new()) {
            let mut identity: PCSShareProtectionKeySet = rasn::der::decode(&identity.keyset).unwrap();
            identity.check_checksum();

            for key in &identity.keys {
                keys.push(key.key());
            }
        }

        let mut pcs_keys = vec![master_key];
        pcs_keys.extend(identities.symm_keys.unwrap_or_default().into_iter().map(|symm| PCSKey(symm.to_vec())));

        Ok((pcs_keys, keys))
    }
}


#[derive(AsnType, Encode, Decode)]
pub struct PCSObjectSignature {
    roll_count: u32,
    // this is a guess, it tracks with the heuristics i've collected
    // but i don't know.
    outer_sign_key_type: u32,
    public: PCSKeyRef,
    signature: PCSSignature,
    // the ignore fields show up in weird situations, when there are multiple keys?
    #[rasn(tag(explicit(context, 0)))]
    symm_key_count: Option<u32>,
    #[rasn(tag(explicit(context, 1)))]
    signature_2: Option<PCSSignature>,
    #[rasn(tag(explicit(context, 2)))]
    ec_key_list: Option<SequenceOf<PCSKeyRef>>,
    #[rasn(tag(explicit(context, 3)))]
    attributes: Option<SequenceOf<PCSAttribute>>,
}