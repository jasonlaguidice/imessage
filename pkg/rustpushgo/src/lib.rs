pub mod util;
#[cfg(target_os = "macos")]
pub mod local_config;
#[cfg(test)]
mod test_hwinfo;

use std::{collections::HashMap, io::Cursor, path::PathBuf, str::FromStr, sync::Arc};

use icloud_auth::{AppleAccount, FetchedToken};
use keystore::{init_keystore, keystore, software::{NoEncryptor, SoftwareKeystore, SoftwareKeystoreState}};
use log::{debug, error, info, warn};
use rustpush::{
    authenticate_apple, login_apple_delegates, register, APSConnectionResource,
    APSState, Attachment, AttachmentType, ConversationData, EditMessage, IDSNGMIdentity,
    IDSUser, IMClient, LoginDelegate, MADRID_SERVICE, MMCSFile, Message, MessageInst, MessagePart,
    MessageParts, MessageType, NormalMessage, OSConfig, ReactMessage, ReactMessageType,
    Reaction, UnsendMessage, IndexedMessagePart, LinkMeta,
    LPLinkMetadata, RichLinkImageAttachmentSubstitute, NSURL, TokenProvider,
    util::{base64_decode, encode_hex},
};
use omnisette::default_provider;
use std::sync::RwLock;
use tokio::sync::broadcast;
use util::{plist_from_string, plist_to_string};

// ============================================================================
// Wrapper types
// ============================================================================

#[derive(uniffi::Object)]
pub struct WrappedAPSState {
    pub inner: Option<APSState>,
}

#[uniffi::export]
impl WrappedAPSState {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string::<APSState>(&s).ok()),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }
}

#[derive(uniffi::Object)]
pub struct WrappedAPSConnection {
    pub inner: rustpush::APSConnection,
}

#[uniffi::export]
impl WrappedAPSConnection {
    pub fn state(&self) -> Arc<WrappedAPSState> {
        Arc::new(WrappedAPSState {
            inner: Some(self.inner.state.blocking_read().clone()),
        })
    }
}

#[derive(uniffi::Record)]
pub struct IDSUsersWithIdentityRecord {
    pub users: Arc<WrappedIDSUsers>,
    pub identity: Arc<WrappedIDSNGMIdentity>,
    /// TokenProvider for iCloud services (CardDAV, CloudKit, etc.)
    pub token_provider: Option<Arc<WrappedTokenProvider>>,
    /// Persist data for restoring the TokenProvider after restart.
    pub account_persist: Option<AccountPersistData>,
}

/// Data needed to restore a TokenProvider from persisted state.
/// Stored in session.json so it survives database resets.
#[derive(uniffi::Record)]
pub struct AccountPersistData {
    pub username: String,
    pub hashed_password_hex: String,
    pub pet: String,
    pub adsid: String,
    pub dsid: String,
    pub spd_base64: String,
}

#[derive(uniffi::Object)]
pub struct WrappedIDSUsers {
    pub inner: Vec<IDSUser>,
}

#[uniffi::export]
impl WrappedIDSUsers {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string(&s).ok())
                .unwrap_or_default(),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }

    pub fn login_id(&self, i: u64) -> String {
        self.inner[i as usize].user_id.clone()
    }

    pub fn get_handles(&self) -> Vec<String> {
        self.inner.iter()
            .flat_map(|user| {
                user.registration.get("com.apple.madrid")
                    .map(|reg| reg.handles.clone())
                    .unwrap_or_default()
            })
            .collect()
    }

    /// Check that all keystore keys referenced by the user state actually exist.
    /// Returns false if any auth/id keypair alias is missing from the keystore,
    /// which means the keystore was wiped or never migrated and re-login is needed.
    pub fn validate_keystore(&self) -> bool {
        if self.inner.is_empty() {
            return true;
        }
        for user in &self.inner {
            let alias = &user.auth_keypair.private.0;
            if keystore().get_key_type(alias).ok().flatten().is_none() {
                warn!("Keystore key '{}' not found for user '{}' — keystore/state mismatch", alias, user.user_id);
                return false;
            }
        }
        true
    }
}

#[derive(uniffi::Object)]
pub struct WrappedIDSNGMIdentity {
    pub inner: IDSNGMIdentity,
}

#[uniffi::export]
impl WrappedIDSNGMIdentity {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string(&s).ok())
                .unwrap_or_else(|| IDSNGMIdentity::new().expect("Failed to create new identity")),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }
}

#[derive(uniffi::Object)]
pub struct WrappedOSConfig {
    pub config: Arc<dyn OSConfig>,
}

#[uniffi::export]
impl WrappedOSConfig {
    /// Get the device UUID from the underlying OSConfig.
    pub fn get_device_id(&self) -> String {
        self.config.get_device_uuid()
    }
}

#[derive(thiserror::Error, uniffi::Error, Debug)]
pub enum WrappedError {
    #[error("{msg}")]
    GenericError { msg: String },
}

impl From<rustpush::PushError> for WrappedError {
    fn from(e: rustpush::PushError) -> Self {
        WrappedError::GenericError { msg: format!("{}", e) }
    }
}

// ============================================================================
// Token Provider (iCloud auth for CardDAV, CloudKit, etc.)
// ============================================================================

/// Wraps a TokenProvider that manages MobileMe auth tokens with auto-refresh.
/// Used for iCloud services like CardDAV contacts and CloudKit messages.
#[derive(uniffi::Object)]
pub struct WrappedTokenProvider {
    inner: Arc<TokenProvider<omnisette::DefaultAnisetteProvider>>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedTokenProvider {
    /// Get HTTP headers needed for iCloud MobileMe API calls.
    /// Includes Authorization (X-MobileMe-AuthToken) and anisette headers.
    /// Auto-refreshes the mmeAuthToken weekly.
    pub async fn get_icloud_auth_headers(&self) -> Result<HashMap<String, String>, WrappedError> {
        Ok(self.inner.get_icloud_auth_headers().await?)
    }

    /// Get the contacts CardDAV URL from the MobileMe delegate config.
    pub async fn get_contacts_url(&self) -> Result<Option<String>, WrappedError> {
        Ok(self.inner.get_contacts_url().await?)
    }

    /// Get the DSID for this account.
    pub async fn get_dsid(&self) -> Result<String, WrappedError> {
        Ok(self.inner.get_dsid().await?)
    }
}

/// Restore a TokenProvider from persisted account credentials.
/// Used on session restore (when we don't go through the login flow).
#[uniffi::export(async_runtime = "tokio")]
pub async fn restore_token_provider(
    config: &WrappedOSConfig,
    connection: &WrappedAPSConnection,
    username: String,
    hashed_password_hex: String,
    pet: String,
    spd_base64: String,
) -> Result<Arc<WrappedTokenProvider>, WrappedError> {
    let os_config = config.config.clone();
    let conn = connection.inner.clone();

    // Create a fresh anisette provider
    let client_info = os_config.get_gsa_config(&*conn.state.read().await, false);
    let anisette = default_provider(client_info.clone(), PathBuf::from_str("state/anisette").unwrap());

    // Create a new AppleAccount and populate it with persisted state
    let mut account = AppleAccount::new_with_anisette(client_info, anisette)
        .map_err(|e| WrappedError::GenericError { msg: format!("Failed to create account: {}", e) })?;

    account.username = Some(username);

    // Restore hashed password
    let hashed_password = rustpush::util::decode_hex(&hashed_password_hex)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid hashed_password hex: {}", e) })?;
    account.hashed_password = Some(hashed_password);

    // Restore SPD from base64-encoded plist
    let spd_bytes = base64_decode(&spd_base64);
    let spd: plist::Dictionary = plist::from_bytes(&spd_bytes)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid SPD plist: {}", e) })?;
    account.spd = Some(spd);

    // Inject the PET token with a far-future expiration so TokenProvider
    // can use it for refresh_mme() without needing to re-login.
    account.tokens.insert("com.apple.gs.idms.pet".to_string(), icloud_auth::FetchedToken {
        token: pet,
        expiration: std::time::SystemTime::now() + std::time::Duration::from_secs(60 * 60 * 24 * 30), // 30 days
    });

    let account = Arc::new(tokio::sync::Mutex::new(account));
    let token_provider = TokenProvider::new(account, os_config);

    info!("Restored TokenProvider from persisted credentials");

    Ok(Arc::new(WrappedTokenProvider { inner: token_provider }))
}

// ============================================================================
// Message wrapper types (flat structs for uniffi)
// ============================================================================

#[derive(uniffi::Record, Clone)]
pub struct WrappedMessage {
    pub uuid: String,
    pub sender: Option<String>,
    pub text: Option<String>,
    pub subject: Option<String>,
    pub participants: Vec<String>,
    pub group_name: Option<String>,
    pub timestamp_ms: u64,
    pub is_sms: bool,

    // Tapback
    pub is_tapback: bool,
    pub tapback_type: Option<u32>,
    pub tapback_target_uuid: Option<String>,
    pub tapback_target_part: Option<u64>,
    pub tapback_emoji: Option<String>,
    pub tapback_remove: bool,

    // Edit
    pub is_edit: bool,
    pub edit_target_uuid: Option<String>,
    pub edit_part: Option<u64>,
    pub edit_new_text: Option<String>,

    // Unsend
    pub is_unsend: bool,
    pub unsend_target_uuid: Option<String>,
    pub unsend_edit_part: Option<u64>,

    // Rename
    pub is_rename: bool,
    pub new_chat_name: Option<String>,

    // Participant change
    pub is_participant_change: bool,
    pub new_participants: Vec<String>,

    // Attachments
    pub attachments: Vec<WrappedAttachment>,

    // Reply
    pub reply_guid: Option<String>,
    pub reply_part: Option<String>,

    // Typing
    pub is_typing: bool,

    // Read receipt
    pub is_read_receipt: bool,

    // Delivered
    pub is_delivered: bool,

    // Error
    pub is_error: bool,
    pub error_for_uuid: Option<String>,
    pub error_status: Option<u64>,
    pub error_status_str: Option<String>,

    // Peer cache invalidate
    pub is_peer_cache_invalidate: bool,

    // Send delivered flag
    pub send_delivered: bool,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedAttachment {
    pub mime_type: String,
    pub filename: String,
    pub uti_type: String,
    pub size: u64,
    pub is_inline: bool,
    pub inline_data: Option<Vec<u8>>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedConversation {
    pub participants: Vec<String>,
    pub group_name: Option<String>,
    pub sender_guid: Option<String>,
    pub is_sms: bool,
}

impl From<&ConversationData> for WrappedConversation {
    fn from(c: &ConversationData) -> Self {
        Self {
            participants: c.participants.clone(),
            group_name: c.cv_name.clone(),
            sender_guid: c.sender_guid.clone(),
            is_sms: false,
        }
    }
}

impl From<&WrappedConversation> for ConversationData {
    fn from(c: &WrappedConversation) -> Self {
        ConversationData {
            participants: c.participants.clone(),
            cv_name: c.group_name.clone(),
            sender_guid: c.sender_guid.clone(),
            after_guid: None,
        }
    }
}

fn convert_reaction(reaction: &Reaction, enable: bool) -> (Option<u32>, Option<String>, bool) {
    let tapback_type = match reaction {
        Reaction::Heart => Some(0),
        Reaction::Like => Some(1),
        Reaction::Dislike => Some(2),
        Reaction::Laugh => Some(3),
        Reaction::Emphasize => Some(4),
        Reaction::Question => Some(5),
        Reaction::Emoji(_) => Some(6),
        Reaction::Sticker { .. } => Some(7),
    };
    let emoji = match reaction {
        Reaction::Emoji(e) => Some(e.clone()),
        _ => None,
    };
    (tapback_type, emoji, !enable)
}

fn message_inst_to_wrapped(msg: &MessageInst) -> WrappedMessage {
    let conv = msg.conversation.as_ref();

    let mut w = WrappedMessage {
        uuid: msg.id.clone(),
        sender: msg.sender.clone(),
        text: None,
        subject: None,
        participants: conv.map(|c| c.participants.clone()).unwrap_or_default(),
        group_name: conv.and_then(|c| c.cv_name.clone()),
        timestamp_ms: msg.sent_timestamp,
        is_sms: false,
        is_tapback: false,
        tapback_type: None,
        tapback_target_uuid: None,
        tapback_target_part: None,
        tapback_emoji: None,
        tapback_remove: false,
        is_edit: false,
        edit_target_uuid: None,
        edit_part: None,
        edit_new_text: None,
        is_unsend: false,
        unsend_target_uuid: None,
        unsend_edit_part: None,
        is_rename: false,
        new_chat_name: None,
        is_participant_change: false,
        new_participants: vec![],
        attachments: vec![],
        reply_guid: None,
        reply_part: None,
        is_typing: false,
        is_read_receipt: false,
        is_delivered: false,
        is_error: false,
        error_for_uuid: None,
        error_status: None,
        error_status_str: None,
        is_peer_cache_invalidate: false,
        send_delivered: msg.send_delivered,
    };

    match &msg.message {
        Message::Message(normal) => {
            w.text = Some(normal.parts.raw_text());
            w.subject = normal.subject.clone();
            w.reply_guid = normal.reply_guid.clone();
            w.reply_part = normal.reply_part.clone();
            w.is_sms = matches!(normal.service, MessageType::SMS { .. });

            for indexed_part in &normal.parts.0 {
                if let MessagePart::Attachment(att) = &indexed_part.part {
                    let (is_inline, inline_data, size) = match &att.a_type {
                        AttachmentType::Inline(data) => (true, Some(data.clone()), data.len() as u64),
                        AttachmentType::MMCS(mmcs) => (false, None, mmcs.size as u64),
                    };
                    w.attachments.push(WrappedAttachment {
                        mime_type: att.mime.clone(),
                        filename: att.name.clone(),
                        uti_type: att.uti_type.clone(),
                        size,
                        is_inline,
                        inline_data,
                    });
                }
            }

            // Encode rich link as special attachments for the Go side
            if let Some(ref lm) = normal.link_meta {
                let original_url: String = lm.data.original_url.clone().into();
                let url: String = lm.data.url.clone().map(|u| u.into()).unwrap_or_default();
                let title = lm.data.title.clone().unwrap_or_default();
                let summary = lm.data.summary.clone().unwrap_or_default();

                info!("Inbound rich link: original_url={}, url={}, title={:?}, summary={:?}, has_image={}, has_icon={}",
                    original_url, url, title, summary,
                    lm.data.image.is_some(), lm.data.icon.is_some());

                let image_mime = if let Some(ref img) = lm.data.image {
                    img.mime_type.clone()
                } else if let Some(ref icon) = lm.data.icon {
                    icon.mime_type.clone()
                } else {
                    String::new()
                };

                // Metadata: original_url\x01url\x01title\x01summary\x01image_mime
                let meta = format!("{}\x01{}\x01{}\x01{}\x01{}",
                    original_url, url, title, summary, image_mime);
                w.attachments.push(WrappedAttachment {
                    mime_type: "x-richlink/meta".to_string(),
                    filename: String::new(),
                    uti_type: String::new(),
                    size: 0,
                    is_inline: true,
                    inline_data: Some(meta.into_bytes()),
                });

                // Image data (from image or icon)
                let image_data = if let Some(ref img) = lm.data.image {
                    let idx = img.rich_link_image_attachment_substitute_index as usize;
                    lm.attachments.get(idx).cloned()
                } else if let Some(ref icon) = lm.data.icon {
                    let idx = icon.rich_link_image_attachment_substitute_index as usize;
                    lm.attachments.get(idx).cloned()
                } else {
                    None
                };

                if let Some(img_data) = image_data {
                    w.attachments.push(WrappedAttachment {
                        mime_type: "x-richlink/image".to_string(),
                        filename: String::new(),
                        uti_type: String::new(),
                        size: img_data.len() as u64,
                        is_inline: true,
                        inline_data: Some(img_data),
                    });
                }
            }
        }
        Message::React(react) => {
            w.is_tapback = true;
            w.tapback_target_uuid = Some(react.to_uuid.clone());
            w.tapback_target_part = react.to_part;
            match &react.reaction {
                ReactMessageType::React { reaction, enable } => {
                    let (tt, emoji, remove) = convert_reaction(reaction, *enable);
                    w.tapback_type = tt;
                    w.tapback_emoji = emoji;
                    w.tapback_remove = remove;
                }
                ReactMessageType::Extension { .. } => {
                    // Extension reactions (stickers etc.) — mark as tapback
                    w.tapback_type = Some(7);
                }
            }
        }
        Message::Edit(edit) => {
            w.is_edit = true;
            w.edit_target_uuid = Some(edit.tuuid.clone());
            w.edit_part = Some(edit.edit_part);
            w.edit_new_text = Some(edit.new_parts.raw_text());
        }
        Message::Unsend(unsend) => {
            w.is_unsend = true;
            w.unsend_target_uuid = Some(unsend.tuuid.clone());
            w.unsend_edit_part = Some(unsend.edit_part);
        }
        Message::RenameMessage(rename) => {
            w.is_rename = true;
            w.new_chat_name = Some(rename.new_name.clone());
        }
        Message::ChangeParticipants(change) => {
            w.is_participant_change = true;
            w.new_participants = change.new_participants.clone();
        }
        Message::Typing(typing, _) => {
            w.is_typing = *typing;
        }
        Message::Read => {
            w.is_read_receipt = true;
        }
        Message::Delivered => {
            w.is_delivered = true;
        }
        Message::Error(err) => {
            w.is_error = true;
            w.error_for_uuid = Some(err.for_uuid.clone());
            w.error_status = Some(err.status);
            w.error_status_str = Some(err.status_str.clone());
        }
        Message::PeerCacheInvalidate => {
            w.is_peer_cache_invalidate = true;
        }
        _ => {}
    }

    w
}

// ============================================================================
// Callback interfaces
// ============================================================================

#[uniffi::export(callback_interface)]
pub trait MessageCallback: Send + Sync {
    fn on_message(&self, msg: WrappedMessage);
}

#[uniffi::export(callback_interface)]
pub trait UpdateUsersCallback: Send + Sync {
    fn update_users(&self, users: Arc<WrappedIDSUsers>);
}

// ============================================================================
// Top-level functions
// ============================================================================

#[uniffi::export]
pub fn init_logger() {
    if std::env::var("RUST_LOG").is_err() {
        std::env::set_var("RUST_LOG", "info");
    }
    let _ = pretty_env_logger::try_init();

    // Initialize the keystore with a file-backed software keystore.
    // This must be called before any rustpush operations (APNs connect, login, etc.).
    //
    // The keystore lives alongside session.json in the XDG data directory
    // (~/.local/share/mautrix-imessage/) so that all session state is in one
    // place and easy to migrate between machines.
    let xdg_dir = resolve_xdg_data_dir();
    let state_path = format!("{}/keystore.plist", xdg_dir);
    let _ = std::fs::create_dir_all(&xdg_dir);

    // Migrate from the old location (state/keystore.plist relative to working
    // directory) if the new file doesn't exist yet.
    let legacy_path = "state/keystore.plist";
    if !std::path::Path::new(&state_path).exists() {
        if std::path::Path::new(legacy_path).exists() {
            match std::fs::copy(legacy_path, &state_path) {
                Ok(_) => info!(
                    "Migrated keystore from {} to {}",
                    legacy_path, state_path
                ),
                Err(e) => warn!(
                    "Failed to migrate keystore from {} to {}: {}",
                    legacy_path, state_path, e
                ),
            }
        }
    }

    let state: SoftwareKeystoreState = match std::fs::read(&state_path) {
        Ok(data) => plist::from_bytes(&data).unwrap_or_else(|e| {
            warn!("Failed to parse keystore at {}: {} — starting with empty keystore", state_path, e);
            SoftwareKeystoreState::default()
        }),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            info!("No keystore file at {} — starting fresh", state_path);
            SoftwareKeystoreState::default()
        }
        Err(e) => {
            warn!("Failed to read keystore at {}: {} — starting with empty keystore", state_path, e);
            SoftwareKeystoreState::default()
        }
    };
    let path_for_closure = state_path.clone();
    init_keystore(SoftwareKeystore {
        state: RwLock::new(state),
        update_state: Box::new(move |s| {
            let _ = plist::to_file_xml(&path_for_closure, s);
        }),
        encryptor: NoEncryptor,
    });
}

/// Resolve the XDG data directory for mautrix-imessage session state.
/// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share.
/// Returns the full path: <base>/mautrix-imessage
fn resolve_xdg_data_dir() -> String {
    if let Ok(xdg) = std::env::var("XDG_DATA_HOME") {
        if !xdg.is_empty() {
            return format!("{}/mautrix-imessage", xdg);
        }
    }
    if let Some(home) = std::env::var("HOME").ok().filter(|h| !h.is_empty()) {
        return format!("{}/.local/share/mautrix-imessage", home);
    }
    // Last resort — fall back to old relative path
    warn!("Could not determine HOME or XDG_DATA_HOME, using local state directory");
    "state".to_string()
}

/// Create a local macOS config that reads hardware info from IOKit
/// and uses AAAbsintheContext for NAC validation (no SIP disable, no relay needed).
/// Only works on macOS — returns an error on other platforms.
#[uniffi::export]
pub fn create_local_macos_config() -> Result<Arc<WrappedOSConfig>, WrappedError> {
    #[cfg(target_os = "macos")]
    {
        let config = local_config::LocalMacOSConfig::new()
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to read hardware info: {}", e) })?;
        Ok(Arc::new(WrappedOSConfig {
            config: Arc::new(config),
        }))
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err(WrappedError::GenericError {
            msg: "Local macOS config is only available on macOS. Use create_config_from_hardware_key instead.".into(),
        })
    }
}

/// Create a local macOS config with a persisted device ID.
/// Only works on macOS — returns an error on other platforms.
#[uniffi::export]
pub fn create_local_macos_config_with_device_id(device_id: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    #[cfg(target_os = "macos")]
    {
        let config = local_config::LocalMacOSConfig::new()
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to read hardware info: {}", e) })?
            .with_device_id(device_id);
        Ok(Arc::new(WrappedOSConfig {
            config: Arc::new(config),
        }))
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err(WrappedError::GenericError {
            msg: "Local macOS config is only available on macOS. Use create_config_from_hardware_key_with_device_id instead.".into(),
        })
    }
}

/// Create a cross-platform config from a base64-encoded JSON hardware key.
///
/// The hardware key is a JSON-serialized `HardwareConfig` extracted once from
/// a real Mac (e.g., via copper's QR code tool). This config uses the
/// open-absinthe NAC emulator to generate fresh validation data on any platform.
///
/// On macOS this is not needed (use `create_local_macos_config` instead).
/// Building with the `hardware-key` feature links open-absinthe + unicorn.
#[uniffi::export]
pub fn create_config_from_hardware_key(base64_key: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    _create_config_from_hardware_key_inner(base64_key, None)
}

/// Create a cross-platform config from a base64-encoded JSON hardware key
/// with a persisted device ID.
#[uniffi::export]
pub fn create_config_from_hardware_key_with_device_id(base64_key: String, device_id: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    _create_config_from_hardware_key_inner(base64_key, Some(device_id))
}

#[cfg(feature = "hardware-key")]
fn _create_config_from_hardware_key_inner(base64_key: String, device_id: Option<String>) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    use base64::{Engine, engine::general_purpose::STANDARD};
    use rustpush::macos::{MacOSConfig, HardwareConfig};

    // Strip whitespace/newlines that chat clients may insert when pasting
    let clean_key: String = base64_key.chars().filter(|c| !c.is_whitespace()).collect();
    let json_bytes = STANDARD.decode(&clean_key)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid base64: {}", e) })?;

    // Try full MacOSConfig first (from extract-key tool), fall back to bare HardwareConfig
    let (hw, nac_relay_url, relay_token, relay_cert_fp) = if let Ok(full) = serde_json::from_slice::<MacOSConfig>(&json_bytes) {
        (full.inner, full.nac_relay_url, full.relay_token, full.relay_cert_fp)
    } else {
        let hw: HardwareConfig = serde_json::from_slice(&json_bytes)
            .map_err(|e| WrappedError::GenericError { msg: format!("Invalid hardware key JSON: {}", e) })?;
        (hw, None, None, None)
    };

    // Always use the real hardware UUID from the extracted key so the bridge
    // shows up as the original Mac rather than a new phantom device.
    // Ignore any persisted device ID — it may be a stale random UUID.
    let hw_uuid = hw.platform_uuid.to_uppercase();
    if let Some(ref old) = device_id {
        if old != &hw_uuid {
            log::warn!(
                "Ignoring persisted device ID {} — using hardware UUID {} from extracted key",
                old, hw_uuid
            );
        }
    }
    let device_id = hw_uuid;

    // Always use known-good values for protocol fields — the extraction tool's
    // values (e.g. icloud_ua) may not match what the runtime code expects.
    let config = MacOSConfig {
        inner: hw,
        version: "15.3".to_string(),
        protocol_version: 1660,
        device_id,
        icloud_ua: "com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0".to_string(),
        aoskit_version: "com.apple.AOSKit/282 (com.apple.accountsd/113)".to_string(),
        udid: None,
        nac_relay_url,
        relay_token,
        relay_cert_fp,
    };

    Ok(Arc::new(WrappedOSConfig {
        config: Arc::new(config),
    }))
}

#[cfg(not(feature = "hardware-key"))]
fn _create_config_from_hardware_key_inner(base64_key: String, _device_id: Option<String>) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    let _ = base64_key;
    Err(WrappedError::GenericError {
        msg: "Hardware key support not available in this build. \
              On macOS, use the Apple ID login flow instead (which uses native validation). \
              To enable hardware key support, rebuild with: cargo build --features hardware-key".into(),
    })
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn connect(
    config: &WrappedOSConfig,
    state: &WrappedAPSState,
) -> Arc<WrappedAPSConnection> {
    let config = config.config.clone();
    let state = state.inner.clone();
    let (connection, error) = APSConnectionResource::new(config, state).await;
    if let Some(error) = error {
        error!("APS connection error (non-fatal, will retry): {}", error);
    }
    Arc::new(WrappedAPSConnection { inner: connection })
}

/// Login session object that holds state between login steps.
#[derive(uniffi::Object)]
pub struct LoginSession {
    account: tokio::sync::Mutex<Option<AppleAccount<omnisette::DefaultAnisetteProvider>>>,
    username: String,
    password_hash: Vec<u8>,
    needs_2fa: bool,
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn login_start(
    apple_id: String,
    password: String,
    config: &WrappedOSConfig,
    connection: &WrappedAPSConnection,
) -> Result<Arc<LoginSession>, WrappedError> {
    let os_config = config.config.clone();
    let conn = connection.inner.clone();

    let user_trimmed = apple_id.trim().to_string();
    // Apple's GSA SRP expects the password to be pre-hashed with SHA-256.
    // See upstream test.rs: sha256(password.as_bytes())
    let pw_bytes = {
        use sha2::{Sha256, Digest};
        let mut hasher = Sha256::new();
        hasher.update(password.trim().as_bytes());
        hasher.finalize().to_vec()
    };

    let client_info = os_config.get_gsa_config(&*conn.state.read().await, false);
    let anisette = default_provider(client_info.clone(), PathBuf::from_str("state/anisette").unwrap());

    let mut account = AppleAccount::new_with_anisette(client_info, anisette)
        .map_err(|e| WrappedError::GenericError { msg: format!("Failed to create account: {}", e) })?;

    let result = account.login_email_pass(&user_trimmed, &pw_bytes).await
        .map_err(|e| WrappedError::GenericError { msg: format!("Login failed: {}", e) })?;

    info!("login_email_pass returned: {:?}", result);
    let needs_2fa = match result {
        icloud_auth::LoginState::LoggedIn => {
            info!("Login completed without 2FA");
            false
        }
        icloud_auth::LoginState::Needs2FAVerification => {
            info!("2FA required (Needs2FAVerification — push already sent by Apple)");
            true
        }
        icloud_auth::LoginState::NeedsDevice2FA | icloud_auth::LoginState::NeedsSMS2FA => {
            info!("2FA required — sending trusted device push");
            match account.send_2fa_to_devices().await {
                Ok(_) => info!("send_2fa_to_devices succeeded"),
                Err(e) => error!("send_2fa_to_devices failed: {}", e),
            }
            true
        }
        icloud_auth::LoginState::NeedsSMS2FAVerification(_) => {
            info!("2FA required (NeedsSMS2FAVerification — SMS already sent)");
            true
        }
        icloud_auth::LoginState::NeedsExtraStep(ref step) => {
            if account.get_pet().is_some() {
                info!("Login completed (extra step ignored, PET available)");
                false
            } else {
                return Err(WrappedError::GenericError { msg: format!("Login requires extra step: {}", step) });
            }
        }
        icloud_auth::LoginState::NeedsLogin => {
            return Err(WrappedError::GenericError { msg: "Login failed - bad credentials".to_string() });
        }
    };

    Ok(Arc::new(LoginSession {
        account: tokio::sync::Mutex::new(Some(account)),
        username: user_trimmed,
        password_hash: pw_bytes,
        needs_2fa,
    }))
}

#[uniffi::export(async_runtime = "tokio")]
impl LoginSession {
    pub fn needs_2fa(&self) -> bool {
        self.needs_2fa
    }

    pub async fn submit_2fa(&self, code: String) -> Result<bool, WrappedError> {
        let mut guard = self.account.lock().await;
        let account = guard.as_mut().ok_or(WrappedError::GenericError { msg: "No active session".to_string() })?;

        info!("Verifying 2FA code via trusted device endpoint (verify_2fa)");
        let result = account.verify_2fa(code).await
            .map_err(|e| WrappedError::GenericError { msg: format!("2FA verification failed: {}", e) })?;

        info!("2FA verification returned: {:?}", result);
        info!("PET token available: {}", account.get_pet().is_some());

        match result {
            icloud_auth::LoginState::LoggedIn => Ok(true),
            icloud_auth::LoginState::NeedsExtraStep(_) => {
                Ok(account.get_pet().is_some())
            }
            _ => Ok(false),
        }
    }

    pub async fn finish(
        &self,
        config: &WrappedOSConfig,
        connection: &WrappedAPSConnection,
        existing_identity: Option<Arc<WrappedIDSNGMIdentity>>,
        existing_users: Option<Arc<WrappedIDSUsers>>,
    ) -> Result<IDSUsersWithIdentityRecord, WrappedError> {
        let os_config = config.config.clone();
        let conn = connection.inner.clone();

        let mut guard = self.account.lock().await;
        let account = guard.as_mut().ok_or(WrappedError::GenericError { msg: "No active session".to_string() })?;

        let pet = account.get_pet()
            .ok_or(WrappedError::GenericError { msg: "No PET token available after login".to_string() })?;

        let spd = account.spd.as_ref().expect("No SPD after login");
        let adsid = spd.get("adsid").expect("No adsid").as_string().unwrap().to_string();
        let dsid = spd.get("DsPrsId").or_else(|| spd.get("dsid"))
            .and_then(|v| {
                if let Some(s) = v.as_string() {
                    Some(s.to_string())
                } else if let Some(i) = v.as_signed_integer() {
                    Some(i.to_string())
                } else if let Some(i) = v.as_unsigned_integer() {
                    Some(i.to_string())
                } else {
                    None
                }
            })
            .unwrap_or_default();

        // Build persist data before delegates call (while we have SPD access)
        let hashed_password_hex = account.hashed_password.as_ref()
            .map(|p| encode_hex(p))
            .unwrap_or_default();
        let mut spd_bytes = Vec::new();
        plist::to_writer_binary(&mut spd_bytes, spd)
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to serialize SPD: {}", e) })?;
        let spd_base64 = rustpush::util::base64_encode(&spd_bytes);

        let account_persist = AccountPersistData {
            username: self.username.clone(),
            hashed_password_hex,
            pet: pet.clone(),
            adsid: adsid.clone(),
            dsid: dsid.clone(),
            spd_base64,
        };

        // Request both IDS (for messaging) and MobileMe (for contacts CardDAV URL)
        let delegates = login_apple_delegates(
            &self.username,
            &pet,
            &adsid,
            None,
            &mut *account.anisette.lock().await,
            &*os_config,
            &[LoginDelegate::IDS, LoginDelegate::MobileMe],
        ).await.map_err(|e| WrappedError::GenericError { msg: format!("Failed to get delegates: {}", e) })?;

        let ids_delegate = delegates.ids.ok_or(WrappedError::GenericError { msg: "No IDS delegate in response".to_string() })?;
        let fresh_user = authenticate_apple(ids_delegate, &*os_config).await
            .map_err(|e| WrappedError::GenericError { msg: format!("IDS authentication failed: {}", e) })?;

        // Resolve identity: reuse existing or generate new
        let identity = match existing_identity {
            Some(wrapped) => {
                info!("Reusing existing identity (avoiding new device notification)");
                wrapped.inner.clone()
            }
            None => {
                info!("Generating new identity (first login)");
                IDSNGMIdentity::new()
                    .map_err(|e| WrappedError::GenericError {
                        msg: format!("Failed to create identity: {}", e)
                    })?
            }
        };

        // Decide whether to reuse existing registration or register fresh.
        let users = match existing_users {
            Some(ref wrapped) if !wrapped.inner.is_empty() => {
                let has_valid_registration = wrapped.inner[0]
                    .registration.get("com.apple.madrid")
                    .map(|r| r.calculate_rereg_time_s().map(|t| t > 0).unwrap_or(false))
                    .unwrap_or(false);

                if has_valid_registration {
                    info!("Reusing existing registration (still valid, skipping register endpoint)");
                    let mut existing = wrapped.inner.clone();
                    existing[0].auth_keypair = fresh_user.auth_keypair.clone();
                    existing
                } else {
                    info!("Existing registration expired, must re-register");
                    let mut users = vec![fresh_user];
                    register(
                        &*os_config,
                        &*conn.state.read().await,
                        &[&MADRID_SERVICE],
                        &mut users,
                        &identity,
                    ).await.map_err(|e| WrappedError::GenericError { msg: format!("Registration failed: {}", e) })?;
                    users
                }
            }
            _ => {
                let mut users = vec![fresh_user];
                if users[0].registration.is_empty() {
                    info!("Registering identity (first login)...");
                    register(
                        &*os_config,
                        &*conn.state.read().await,
                        &[&MADRID_SERVICE],
                        &mut users,
                        &identity,
                    ).await.map_err(|e| WrappedError::GenericError { msg: format!("Registration failed: {}", e) })?;
                }
                users
            }
        };

        // Take ownership of the account to create a TokenProvider.
        // The MobileMe delegate from `delegates` is seeded into the provider
        // so the first get_mme_token() doesn't need to re-fetch.
        let owned_account = guard.take()
            .ok_or(WrappedError::GenericError { msg: "Account already consumed".to_string() })?;
        let account_arc = Arc::new(tokio::sync::Mutex::new(owned_account));
        let token_provider = TokenProvider::new(account_arc, os_config.clone());

        // Seed the MobileMe delegate so get_contacts_url() and get_mme_token()
        // work immediately without a network round-trip.
        if let Some(mobileme) = delegates.mobileme {
            token_provider.seed_mme_delegate(mobileme).await;
        }

        Ok(IDSUsersWithIdentityRecord {
            users: Arc::new(WrappedIDSUsers { inner: users }),
            identity: Arc::new(WrappedIDSNGMIdentity { inner: identity }),
            token_provider: Some(Arc::new(WrappedTokenProvider { inner: token_provider })),
            account_persist: Some(account_persist),
        })
    }
}

// ============================================================================
// Attachment download helper
// ============================================================================

/// Download any MMCS (non-inline) attachments from the message and convert them
/// to inline data in the wrapped message, so the Go side can upload them to Matrix.
async fn download_mmcs_attachments(
    wrapped: &mut WrappedMessage,
    msg_inst: &MessageInst,
    conn: &rustpush::APSConnectionResource,
) {
    if let Message::Message(normal) = &msg_inst.message {
        let mut att_idx = 0;
        for indexed_part in &normal.parts.0 {
            if let MessagePart::Attachment(att) = &indexed_part.part {
                if let AttachmentType::MMCS(_) = &att.a_type {
                    if att_idx < wrapped.attachments.len() {
                        let mut buf: Vec<u8> = Vec::new();
                        match att.get_attachment(conn, &mut buf, |_, _| {}).await {
                            Ok(()) => {
                                info!(
                                    "Downloaded MMCS attachment: {} ({} bytes)",
                                    att.name,
                                    buf.len()
                                );
                                wrapped.attachments[att_idx].is_inline = true;
                                wrapped.attachments[att_idx].inline_data = Some(buf);
                            }
                            Err(e) => {
                                error!("Failed to download MMCS attachment {}: {}", att.name, e);
                            }
                        }
                    }
                }
                att_idx += 1;
            }
        }
    }
}

// ============================================================================
// Client
// ============================================================================

#[derive(uniffi::Object)]
pub struct Client {
    client: Arc<IMClient>,
    conn: rustpush::APSConnection,
    receive_handle: tokio::sync::Mutex<Option<tokio::task::JoinHandle<()>>>,
    token_provider: Option<Arc<WrappedTokenProvider>>,
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn new_client(
    connection: &WrappedAPSConnection,
    users: &WrappedIDSUsers,
    identity: &WrappedIDSNGMIdentity,
    config: &WrappedOSConfig,
    token_provider: Option<Arc<WrappedTokenProvider>>,
    message_callback: Box<dyn MessageCallback>,
    update_users_callback: Box<dyn UpdateUsersCallback>,
) -> Result<Arc<Client>, WrappedError> {
    let conn = connection.inner.clone();
    let users_clone = users.inner.clone();
    let identity_clone = identity.inner.clone();
    let config_clone = config.config.clone();

    let _ = std::fs::create_dir_all("state");

    let client = Arc::new(
        IMClient::new(
            conn.clone(),
            users_clone,
            identity_clone,
            &[&MADRID_SERVICE],
            "state/id_cache.plist".into(),
            config_clone,
            Box::new(move |updated_keys| {
                update_users_callback.update_users(Arc::new(WrappedIDSUsers {
                    inner: updated_keys,
                }));
                debug!("Updated IDS keys");
            }),
        )
        .await,
    );

    // Start receive loop
    let client_for_recv = client.clone();
    let callback = Arc::new(message_callback);

    let receive_handle = tokio::spawn({
        let conn = connection.inner.clone();
        let conn_for_download = connection.inner.clone();
        async move {
            let mut recv = conn.messages_cont.subscribe();
            loop {
                match recv.recv().await {
                    Ok(msg) => {
                        match client_for_recv.handle(msg).await {
                            Ok(Some(msg_inst)) => {
                                if msg_inst.has_payload() || matches!(msg_inst.message, Message::Typing(_, _) | Message::Read | Message::Delivered | Message::Error(_) | Message::PeerCacheInvalidate) {
                                    let mut wrapped = message_inst_to_wrapped(&msg_inst);
                                    // Download MMCS attachments so Go receives inline data
                                    download_mmcs_attachments(&mut wrapped, &msg_inst, &conn_for_download).await;
                                    callback.on_message(wrapped);
                                }
                            }
                            Ok(None) => {}
                            Err(e) => {
                                error!("Error handling message: {:?}", e);
                            }
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(n)) => {
                        warn!("Message receiver lagged by {} messages", n);
                    }
                    Err(broadcast::error::RecvError::Closed) => {
                        info!("Message channel closed, stopping receive loop");
                        break;
                    }
                }
            }
        }
    });

    Ok(Arc::new(Client {
        client,
        conn: connection.inner.clone(),
        receive_handle: tokio::sync::Mutex::new(Some(receive_handle)),
        token_provider,
    }))
}

#[uniffi::export(async_runtime = "tokio")]
impl Client {
    pub async fn get_handles(&self) -> Vec<String> {
        self.client.identity.get_handles().await
    }

    /// Get iCloud auth headers (Authorization + anisette) for MobileMe API calls.
    /// Returns None if no token provider is available.
    pub async fn get_icloud_auth_headers(&self) -> Result<Option<HashMap<String, String>>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(Some(tp.inner.get_icloud_auth_headers().await?)),
            None => Ok(None),
        }
    }

    /// Get the contacts CardDAV URL from the MobileMe delegate.
    /// Returns None if no token provider is available.
    pub async fn get_contacts_url(&self) -> Result<Option<String>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(tp.inner.get_contacts_url().await?),
            None => Ok(None),
        }
    }

    /// Get the DSID for this account.
    pub async fn get_dsid(&self) -> Result<Option<String>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(Some(tp.inner.get_dsid().await?)),
            None => Ok(None),
        }
    }

    pub async fn validate_targets(
        &self,
        targets: Vec<String>,
        handle: String,
    ) -> Vec<String> {
        self.client
            .identity
            .validate_targets(&targets, "com.apple.madrid", &handle)
            .await
            .unwrap_or_default()
    }

    pub async fn send_message(
        &self,
        conversation: WrappedConversation,
        text: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let service = if conversation.is_sms {
            MessageType::SMS {
                is_phone: false,
                using_number: handle.clone(),
                from_handle: None,
            }
        } else {
            MessageType::IMessage
        };

        // Parse rich link encoded as prefix: \x00RL\x01original_url\x01url\x01title\x01summary\x00actual_text
        let (actual_text, link_meta) = if text.starts_with("\x00RL\x01") {
            let rest = &text[4..]; // skip "\x00RL\x01"
            if let Some(end) = rest.find('\x00') {
                let metadata = &rest[..end];
                let actual = rest[end + 1..].to_string();
                let fields: Vec<&str> = metadata.splitn(4, '\x01').collect();
                let original_url_str = fields.first().copied().unwrap_or("");
                let url_str = fields.get(1).copied().unwrap_or("");
                let title_str = fields.get(2).copied().unwrap_or("");
                let summary_str = fields.get(3).copied().unwrap_or("");

                let original_url = NSURL {
                    base: "$null".to_string(),
                    relative: original_url_str.to_string(),
                };
                let url = if url_str.is_empty() {
                    None
                } else {
                    Some(NSURL {
                        base: "$null".to_string(),
                        relative: url_str.to_string(),
                    })
                };
                let title = if title_str.is_empty() { None } else { Some(title_str.to_string()) };
                let summary = if summary_str.is_empty() { None } else { Some(summary_str.to_string()) };

                info!("Sending rich link: url={}, title={:?}", original_url_str, title);

                let lm = LinkMeta {
                    data: LPLinkMetadata {
                        image_metadata: None,
                        version: 1,
                        icon_metadata: None,
                        original_url,
                        url,
                        title,
                        summary,
                        image: None,
                        icon: None,
                        images: None,
                        icons: None,
                    },
                    attachments: vec![],
                };
                (actual, Some(lm))
            } else {
                (text, None)
            }
        } else {
            (text, None)
        };

        let mut normal = NormalMessage::new(actual_text.clone(), service);
        normal.link_meta = link_meta;
        let mut msg = MessageInst::new(
            conv.clone(),
            &handle,
            Message::Message(normal),
        );
        match self.client.send(&mut msg).await {
            Ok(_) => Ok(msg.id.clone()),
            Err(rustpush::PushError::NoValidTargets) if !conversation.is_sms => {
                // iMessage failed — no IDS targets. Retry as SMS (without rich link).
                info!("No IDS targets, falling back to SMS for {:?}", conv.participants);
                let sms_service = MessageType::SMS {
                    is_phone: false,
                    using_number: handle.clone(),
                    from_handle: None,
                };
                let mut sms_msg = MessageInst::new(
                    conv,
                    &handle,
                    Message::Message(NormalMessage::new(actual_text, sms_service)),
                );
                self.client.send(&mut sms_msg).await
                    .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS: {}", e) })?;
                Ok(sms_msg.id.clone())
            }
            Err(e) => Err(WrappedError::GenericError { msg: format!("Failed to send message: {}", e) }),
        }
    }

    pub async fn send_tapback(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        target_part: u64,
        reaction: u32,
        emoji: Option<String>,
        remove: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let reaction_val = match (reaction, &emoji) {
            (0, _) => Reaction::Heart,
            (1, _) => Reaction::Like,
            (2, _) => Reaction::Dislike,
            (3, _) => Reaction::Laugh,
            (4, _) => Reaction::Emphasize,
            (5, _) => Reaction::Question,
            (6, Some(em)) => Reaction::Emoji(em.clone()),
            _ => Reaction::Heart,
        };
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::React(ReactMessage {
                to_uuid: target_uuid,
                to_part: Some(target_part),
                reaction: ReactMessageType::React { reaction: reaction_val, enable: !remove },
                to_text: String::new(),
                embedded_profile: None,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send tapback: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_typing(
        &self,
        conversation: WrappedConversation,
        typing: bool,
        handle: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Typing(typing, None));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send typing: {}", e) })?;
        Ok(())
    }

    pub async fn send_read_receipt(
        &self,
        conversation: WrappedConversation,
        handle: String,
        for_uuid: Option<String>,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Read);
        if let Some(uuid) = for_uuid {
            msg.id = uuid;
        }
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send read receipt: {}", e) })?;
        Ok(())
    }

    pub async fn send_delivery_receipt(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Delivered);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send delivery receipt: {}", e) })?;
        Ok(())
    }

    pub async fn send_edit(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        edit_part: u64,
        new_text: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Edit(EditMessage {
                tuuid: target_uuid,
                edit_part,
                new_parts: MessageParts(vec![IndexedMessagePart {
                    part: MessagePart::Text(new_text, Default::default()),
                    idx: None,
                    ext: None,
                }]),
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send edit: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_unsend(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        edit_part: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Unsend(UnsendMessage {
                tuuid: target_uuid,
                edit_part,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send unsend: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_attachment(
        &self,
        conversation: WrappedConversation,
        data: Vec<u8>,
        mime: String,
        uti_type: String,
        filename: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        // Detect voice messages by UTI (CAF files from OGG→CAF remux are voice recordings)
        let is_voice = uti_type == "com.apple.coreaudio-format";
        let service = if conversation.is_sms {
            MessageType::SMS {
                is_phone: false,
                using_number: handle.clone(),
                from_handle: None,
            }
        } else {
            MessageType::IMessage
        };

        // Prepare and upload the attachment via MMCS
        let cursor = Cursor::new(&data);
        let prepared = MMCSFile::prepare_put(cursor).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to prepare MMCS upload: {}", e) })?;

        let cursor2 = Cursor::new(&data);
        let attachment = Attachment::new_mmcs(
            &self.conn,
            &prepared,
            cursor2,
            &mime,
            &uti_type,
            &filename,
            |_current, _total| {},
        ).await.map_err(|e| WrappedError::GenericError { msg: format!("Failed to upload attachment: {}", e) })?;

        let parts = vec![IndexedMessagePart {
            part: MessagePart::Attachment(attachment.clone()),
            idx: None,
            ext: None,
        }];

        let mut msg = MessageInst::new(
            conv.clone(),
            &handle,
            Message::Message(NormalMessage {
                parts: MessageParts(parts),
                effect: None,
                reply_guid: None,
                reply_part: None,
                service,
                subject: None,
                app: None,
                link_meta: None,
                voice: is_voice,
                scheduled: None,
                embedded_profile: None,
            }),
        );
        match self.client.send(&mut msg).await {
            Ok(_) => Ok(msg.id.clone()),
            Err(rustpush::PushError::NoValidTargets) if !conversation.is_sms => {
                info!("No IDS targets for attachment, falling back to SMS for {:?}", conv.participants);
                let sms_service = MessageType::SMS {
                    is_phone: false,
                    using_number: handle.clone(),
                    from_handle: None,
                };
                let sms_parts = vec![IndexedMessagePart {
                    part: MessagePart::Attachment(attachment),
                    idx: None,
                    ext: None,
                }];
                let mut sms_msg = MessageInst::new(
                    conv,
                    &handle,
                    Message::Message(NormalMessage {
                        parts: MessageParts(sms_parts),
                        effect: None,
                        reply_guid: None,
                        reply_part: None,
                        service: sms_service,
                        subject: None,
                        app: None,
                        link_meta: None,
                        voice: is_voice,
                        scheduled: None,
                        embedded_profile: None,
                    }),
                );
                self.client.send(&mut sms_msg).await
                    .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS attachment: {}", e) })?;
                Ok(sms_msg.id.clone())
            }
            Err(e) => Err(WrappedError::GenericError { msg: format!("Failed to send attachment: {}", e) }),
        }
    }

    pub async fn stop(&self) {
        let mut handle = self.receive_handle.lock().await;
        if let Some(h) = handle.take() {
            h.abort();
        }
    }
}

impl Drop for Client {
    fn drop(&mut self) {
        if let Ok(mut handle) = self.receive_handle.try_lock() {
            if let Some(h) = handle.take() {
                h.abort();
            }
        }
    }
}

uniffi::setup_scaffolding!();
