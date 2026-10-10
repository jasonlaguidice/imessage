//! CloudKit message records decoded the pre-0e0f13c way.
//!
//! rustpush 0e0f13c turned `CloudMessage` into a decoded enum that discards
//! the body of msgType-0 records (ordinary user messages as CloudKit stores
//! them). The bridge reads `messageManateeZone` through this module instead,
//! so backfill keeps those bodies without patching rustpush.

use std::collections::HashMap;

use crate::BridgeDefaultAnisetteProvider;

/// A CloudKit `MessageEncryptedV3` record decoded the way rustpush did it
/// before 0e0f13c ("More fixes").
///
/// Upstream now turns the raw record (`RemoteCloudMessage`) into a
/// `CloudMessage` whose body is a `CloudMessageType` enum keyed on `msgType`:
/// only 1..=2 decode as a message, 3..=7 decode as other protos with
/// `.unwrap()` (a malformed payload panics), and everything else, including
/// msgType 0, becomes `Unknown` with the body discarded. CloudKit delivers
/// ordinary user messages as msgType 0, so going through upstream's type would
/// silently drop message bodies from backfill. This view keeps the old shape:
/// every record's `msgProto*` payloads decode as the message protos with the
/// same lenient decode (`CloudKitBytes::from_bytes`, default on error) and the
/// raw `msgType` is passed through to Go unchanged.
#[allow(dead_code)]
#[derive(Debug, Clone)]
pub(crate) struct LegacyCloudMessage {
    pub(crate) r#type: i64,
    pub(crate) utm: Option<std::time::SystemTime>,
    pub(crate) error: i64,
    pub(crate) chat_id: String,
    pub(crate) sender: String,
    pub(crate) time: i64,
    pub(crate) destination_caller_id: String,
    pub(crate) flags: rustpush::cloud_messages::MessageFlags,
    pub(crate) guid: String,
    pub(crate) service: String,
    pub(crate) msg_proto: rustpush::cloud_messages::cloudmessagesp::MessageProto,
    pub(crate) msg_proto_2: Option<rustpush::cloud_messages::cloudmessagesp::MessageProto2>,
    pub(crate) msg_proto_3: Option<rustpush::cloud_messages::cloudmessagesp::MessageProto3>,
    pub(crate) msg_proto_4: Option<rustpush::cloud_messages::cloudmessagesp::MessageProto4>,
}

impl From<rustpush::cloud_messages::RemoteCloudMessage> for LegacyCloudMessage {
    fn from(raw: rustpush::cloud_messages::RemoteCloudMessage) -> Self {
        use rustpush::cloudkit_proto::CloudKitBytes;
        Self {
            r#type: raw.r#type,
            utm: raw.utm,
            error: raw.error,
            chat_id: raw.chat_id,
            sender: raw.sender,
            time: raw.time,
            destination_caller_id: raw.destination_caller_id,
            flags: raw.flags,
            guid: raw.guid,
            service: raw.service,
            msg_proto: CloudKitBytes::from_bytes(raw.msg_proto.0),
            msg_proto_2: raw.msg_proto_2.map(|p| CloudKitBytes::from_bytes(p.0)),
            msg_proto_3: raw.msg_proto_3.map(|p| CloudKitBytes::from_bytes(p.0)),
            msg_proto_4: raw.msg_proto_4.map(|p| CloudKitBytes::from_bytes(p.0)),
        }
    }
}

/// One page of `messageManateeZone`, decoded as `LegacyCloudMessage`.
///
/// Mirrors upstream's private `CloudMessagesClient::sync_records` (the body of
/// `sync_messages`) request for request: same RetrieveChangesRequest (change
/// types 3, newest-first, no assets), same PCSRecordKeyMissing handling (drop
/// the cached zone key and return the error so callers run PCS recovery). Only
/// the per-record decode differs; see `LegacyCloudMessage`.
pub(crate) async fn sync_cloud_messages_legacy(
    cloud_messages: &rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>,
    continuation_token: Option<Vec<u8>>,
) -> Result<(Vec<u8>, HashMap<String, Option<LegacyCloudMessage>>, i32), rustpush::PushError> {
    use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
    use rustpush::cloudkit_proto::CloudKitRecord;
    use rustpush::cloud_messages::{MESSAGES_SERVICE, RemoteCloudMessage};

    let container = cloud_messages.get_container().await?;
    let zone = container.private_zone("messageManateeZone".to_string());
    let key = container.get_zone_encryption_config(&zone, &cloud_messages.keychain, &MESSAGES_SERVICE).await?;
    let (_assets, response) = container.perform(&CloudKitSession::new(),
        FetchRecordChangesOperation(rustpush::cloudkit_proto::RetrieveChangesRequest {
            sync_continuation_token: continuation_token,
            zone_identifier: Some(zone.clone()),
            requested_changes_types: Some(3),
            assets_to_download: Some(NO_ASSETS.clone()),
            newest_first: Some(true),
            ..Default::default()
        })).await?;

    let mut results = HashMap::new();
    for change in &response.change {
        let identifier = change.identifier.as_ref().unwrap().value.as_ref().unwrap().name().to_string();
        let Some(record) = &change.record else {
            results.insert(identifier, None);
            continue;
        };
        if record.r#type.as_ref().unwrap().name() != RemoteCloudMessage::record_type() { continue }

        let pcskey = match pcs_keys_for_record(record, &key) {
            Ok(key) => key,
            Err(rustpush::PushError::PCSRecordKeyMissing) => {
                container.clear_cache_zone_encryption_config(&zone).await;
                return Err(rustpush::PushError::PCSRecordKeyMissing)
            },
            Err(e) => return Err(e),
        };
        let item = RemoteCloudMessage::from_record_encrypted(&record.record_field, Some(&pcskey));
        results.insert(identifier, Some(LegacyCloudMessage::from(item)));
    }

    Ok((response.sync_continuation_token().to_vec(), results, response.status()))
}

#[cfg(test)]
mod tests {
    use super::LegacyCloudMessage;
    use rustpush::cloud_messages::cloudmessagesp::{MessageProto, MessageProto4};
    use rustpush::cloud_messages::{CloudMessage, CloudMessageType, GZipWrapper, MessageFlags, RemoteCloudMessage};
    use rustpush::cloudkit_proto::CloudKitBytes;

    /// A msgProto payload as CloudKit stores it (a gzipped protobuf), decoded
    /// through upstream's own `GZipWrapper` codec: the same step
    /// `RemoteCloudMessage::from_record_encrypted` runs on the record field.
    fn stored_payload<M: prost::Message>(proto: &M) -> GZipWrapper<Vec<u8>> {
        let on_the_wire = GZipWrapper(proto.encode_to_vec()).to_bytes();
        GZipWrapper::<Vec<u8>>::from_bytes(on_the_wire)
    }

    /// An ordinary user message as CloudKit hands it over: msgType 0.
    fn msg_type_0_record() -> RemoteCloudMessage {
        let body = MessageProto {
            version: 1,
            text: Some("hello from backfill".to_string()),
            subject: Some("subj".to_string()),
            attributed_body: Some(vec![1, 2, 3]),
            date_read: Some(42),
            ..Default::default()
        };
        let extras = MessageProto4 {
            associated_message_emoji: Some("🎉".to_string()),
            ..Default::default()
        };
        RemoteCloudMessage {
            r#type: 0,
            chat_id: "iMessage;-;+15555550100".to_string(),
            sender: "tel:+15555550100".to_string(),
            time: 1_000,
            flags: MessageFlags::IS_FROM_ME,
            guid: "ABCDEF01-2345-6789-ABCD-EF0123456789".to_string(),
            service: "iMessage".to_string(),
            msg_proto: stored_payload(&body),
            msg_proto_4: Some(stored_payload(&extras)),
            ..Default::default()
        }
    }

    #[test]
    fn msg_type_0_keeps_its_body() {
        let legacy = LegacyCloudMessage::from(msg_type_0_record());

        assert_eq!(legacy.r#type, 0, "msgType must reach Go unchanged");
        assert_eq!(legacy.msg_proto.text.as_deref(), Some("hello from backfill"));
        assert_eq!(legacy.msg_proto.subject.as_deref(), Some("subj"));
        assert_eq!(legacy.msg_proto.attributed_body.as_deref(), Some(&[1u8, 2, 3][..]));
        assert_eq!(legacy.msg_proto.date_read, Some(42));
        assert_eq!(
            legacy.msg_proto_4.and_then(|p| p.associated_message_emoji).as_deref(),
            Some("🎉")
        );
        assert_eq!(legacy.guid, "ABCDEF01-2345-6789-ABCD-EF0123456789");
        assert!(legacy.flags.contains(MessageFlags::IS_FROM_ME));
    }

    /// Documents the trap this module exists for: upstream's decoded type maps
    /// msgType 0 to a non-message variant and the body is gone. If this starts
    /// failing, upstream changed the mapping and the legacy view may be
    /// retirable.
    #[test]
    fn upstream_cloud_message_drops_msg_type_0_bodies() {
        let upstream = CloudMessage::from(msg_type_0_record());
        assert!(
            !matches!(upstream.message, CloudMessageType::Message { .. }),
            "upstream now decodes msgType 0 as a message; re-evaluate cloud_message_legacy"
        );
    }

    #[test]
    fn malformed_body_decodes_to_default_instead_of_panicking() {
        let mut record = msg_type_0_record();
        record.msg_proto = GZipWrapper(vec![0xff, 0xff, 0xff]);
        let legacy = LegacyCloudMessage::from(record);
        assert_eq!(legacy.msg_proto.text, None);
    }
}
