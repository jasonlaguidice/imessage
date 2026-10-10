//! Notify-only FaceTime.
//!
//! The bridge stays registered for the FaceTime IDS service and hands every
//! APNs message to rustpush's `FTClient::handle()` (from the receive loop, under
//! a panic guard). This module turns the resulting events into the synthetic
//! messages Go renders as call notices: an incoming ring, and a call that went
//! unanswered — either declined, or dropped by the caller while we were still
//! ringing (a LeaveEvent that leaves the session in FTMode::Missed, told apart
//! from an after-the-fact leave by the ringing snapshot below). It also loads
//! the per-login FaceTime state file across the rustpush ad4265a format change.

use crate::{plist_value_as_u64, read_plist_state, WrappedMessage};
use log::{info, warn};

// FaceTime is notify-only. The bridge stays registered for the FaceTime IDS
// service so Apple keeps delivering call events, rustpush's FTClient::handle()
// keeps its session state coherent, and an incoming or missed call becomes one
// m.notice. Nothing else: no join links, no session creation, no ringing, no
// LetMeIn approval, no propping the bridge into a call. Since rustpush ad4265a
// ("Initial FaceTime support") join / create_session / respond_letmein /
// connect_to_relay open a real media link and unwrap local interfaces this
// bridge never sets, so the bridge must never call them. The web-join
// implementation this replaced lives in git history (before the rustpush
// 48bcc27 bump) for the future media-bridging research.
//
// Go recognizes these marker prefixes and renders the notice.
const FACETIME_RING_MARKER: &str = "[[FACETIME_RING]]";
const FACETIME_MISSED_MARKER: &str = "[[FACETIME_MISSED]]";

/// A session's call state just before `FTClient::handle()` ran.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct SessionBefore {
    /// `is_ringing_inaccurate` — our devices were ringing.
    pub ringing: bool,
    /// `mode` was already `FTMode::Missed`, i.e. a LeaveEvent ended the
    /// ring and its missed-call notice has been posted.
    pub missed: bool,
}

/// Which FaceTime sessions were ringing — and which had already been written
/// off as missed — just before an APNs event was handed to
/// `FTClient::handle()`: guid -> [`SessionBefore`]. Taken by the receive loop
/// so a LeaveEvent can be told apart: the caller hanging up while we were
/// still ringing is a missed call; a leave after the call ended is not.
///
/// `ringing` comes from the `#[serde(skip)]` `is_ringing_inaccurate`, so a
/// session restored from disk reads not-ringing; `missed` comes from the
/// persisted `mode`, and rustpush resets it to Incoming when a new call rings
/// on the same guid.
pub(crate) async fn ringing_snapshot(
    facetime: &rustpush::facetime::FTClient,
) -> std::collections::HashMap<String, SessionBefore> {
    let state = facetime.state.read().await;
    state
        .sessions
        .iter()
        .map(|(guid, session)| {
            (
                guid.clone(),
                SessionBefore {
                    ringing: session.is_ringing_inaccurate,
                    missed: matches!(
                        session.mode,
                        Some(rustpush::facetime::FTMode::Missed)
                    ),
                },
            )
        })
        .collect()
}

/// What notice (if any) an inbound FaceTime event becomes, and the sender
/// handle the event itself names. `before` is the session's state before
/// handle() ran (None if the session was unknown), `missed_after` is whether
/// handle() left the session in FTMode::Missed, and `my_handles` lists the
/// handles this bridge itself has in that session.
///
/// `before` and `missed_after` together tell a caller who hung up mid-ring (a
/// missed call) from a member leaving a call that had already stopped ringing
/// here, which would otherwise post a second notice (or a spurious first one,
/// for a session this bridge never saw ring).
pub(crate) fn notice_for_event(
    event: &rustpush::facetime::FTMessage,
    before: Option<SessionBefore>,
    missed_after: bool,
    my_handles: &std::collections::HashSet<String>,
) -> Option<(&'static str, Option<String>)> {
    match event {
        rustpush::facetime::FTMessage::Ring { .. } => Some((FACETIME_RING_MARKER, None)),
        rustpush::facetime::FTMessage::JoinEvent { handle, ring, .. } if *ring => {
            Some((FACETIME_RING_MARKER, Some(handle.clone())))
        }
        rustpush::facetime::FTMessage::AddMembers { members, ring, .. } if *ring => {
            Some((FACETIME_RING_MARKER, members.iter().next().map(|member| member.handle.clone())))
        }
        // Only a session already in FTMode::Missed means the call had ended
        // before this Decline (its LeaveEvent posted the missed notice).
        // Everything else — a session still ringing, one we never saw, and one
        // restored from disk (not ringing, not missed) — makes this Decline
        // the first and only missed-call notice.
        rustpush::facetime::FTMessage::Decline { .. } => {
            if before.map_or(false, |before| before.missed) {
                None
            } else {
                Some((FACETIME_MISSED_MARKER, None))
            }
        }
        // A leave that ends a call still ringing here is the caller hanging
        // up: the session was ringing and handle() moved it to FTMode::Missed.
        // One of our own handles leaving is never a missed call *from* someone.
        // `temp:<uuid>` is a web joiner's pseudonym, not a reachable handle.
        rustpush::facetime::FTMessage::LeaveEvent { handle, .. } => {
            let rang_here = before.map_or(false, |before| before.ringing);
            if rang_here && missed_after && !my_handles.contains(handle) {
                if handle.starts_with("temp:") {
                    Some((FACETIME_MISSED_MARKER, None))
                } else {
                    Some((FACETIME_MISSED_MARKER, Some(handle.clone())))
                }
            } else {
                None
            }
        }
        _ => None,
    }
}

/// Map an inbound FaceTime event to the synthetic message Go turns into a
/// notice: someone is calling, or the call went unanswered — including a
/// LeaveEvent while we were still ringing, i.e. the caller hung up on us.
/// Every other FTMessage is session state the bridge no longer acts on,
/// including RespondedElsewhere: a notify-only bridge never answers, so
/// "answered on another device" would fire for every call the user picks up.
///
/// `ringing_before` is the snapshot taken before `handle()` ran, and is what
/// distinguishes that mid-ring hang-up from someone leaving a finished call.
pub(crate) async fn facetime_event_to_wrapped(
    facetime: &rustpush::facetime::FTClient,
    event: &rustpush::facetime::FTMessage,
    ringing_before: &std::collections::HashMap<String, SessionBefore>,
) -> Option<WrappedMessage> {
    let guid = event_guid(event)?;

    // Two lookups, one state read: the mode handle() left behind (the ringing
    // snapshot only says the session was ringing *before* handle() ran) and the
    // member handles the notice names. `drop(state)` below releases the guard
    // before the decision and the sender fallback, so it is never held across
    // them — the way the pre-9a version of this function already read it.
    let state = facetime.state.read().await;
    let session = state.sessions.get(guid);
    let (missed_after, participants, my_handles) = session
        .map(|session| {
            let participants = session
                .members
                .iter()
                .map(|member| member.handle.clone())
                .filter(|handle| !handle.starts_with("temp:"))
                .collect::<Vec<_>>();
            let my_handles: std::collections::HashSet<String> =
                session.my_handles.iter().cloned().collect();
            (
                matches!(session.mode, Some(rustpush::facetime::FTMode::Missed)),
                participants,
                my_handles,
            )
        })
        .unwrap_or_else(|| (false, Vec::new(), std::collections::HashSet::new()));
    drop(state);

    let (marker, mut sender) =
        notice_for_event(event, ringing_before.get(guid).copied(), missed_after, &my_handles)?;

    // The caller's handle names the notice. When the event doesn't carry one,
    // fall back to the first session member that is neither one of our own
    // handles nor an Apple temp pseudonym (web joiners use temp:<uuid>).
    if sender.is_none() {
        sender = participants
            .iter()
            .find(|participant| {
                !participant.starts_with("temp:") && !my_handles.contains(*participant)
            })
            .cloned();
    }

    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;
    let event_kind = marker.trim_matches('[').trim_matches(']').to_lowercase();
    let mut wrapped = WrappedMessage::default();
    wrapped.uuid = format!("FACETIME_{}_{}_{}", event_kind.to_uppercase(), guid, now_ms);
    wrapped.sender = sender;
    wrapped.text = Some(format!("{marker} guid={guid}"));
    wrapped.participants = participants;
    wrapped.timestamp_ms = now_ms;
    wrapped.is_notify_anyways = true;
    Some(wrapped)
}

/// The session guid an inbound event names, so the caller can look the
/// session up in both the ringing snapshot and the post-handle state read.
/// Every variant but `LetMeInRequest` carries one; that request type and the
/// purely informational variants (Connected, Disconnected, RemoveMembers)
/// never become a notice, so their arms only keep the match exhaustive.
fn event_guid(event: &rustpush::facetime::FTMessage) -> Option<&str> {
    use rustpush::facetime::FTMessage;
    match event {
        FTMessage::LinkChanged { guid }
        | FTMessage::JoinEvent { guid, .. }
        | FTMessage::AddMembers { guid, .. }
        | FTMessage::RemoveMembers { guid, .. }
        | FTMessage::LeaveEvent { guid, .. }
        | FTMessage::Ring { guid }
        | FTMessage::Decline { guid }
        | FTMessage::RespondedElsewhere { guid }
        | FTMessage::Connected { guid }
        | FTMessage::Disconnected { guid } => Some(guid),
        _ => None,
    }
}

/// Load the persisted FaceTime state, migrating plists written before rustpush
/// ad4265a ("Initial FaceTime support").
///
/// That commit added `FTSession::creation_time: u64` with no serde default, so
/// a state file holding any session from the old pin fails to parse as a
/// whole. `read_plist_state` would then fall back to an empty `FTState` and
/// the next persist would overwrite the file. Backfill the field (last rekey,
/// else start time, else now) on the raw plist before the typed parse instead.
pub(crate) fn read_facetime_state(path: &str) -> rustpush::facetime::FTState {
    let Some(mut raw) = read_plist_state::<plist::Value>(path) else {
        return Default::default();
    };
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0);
    let migrated = backfill_facetime_creation_time(&mut raw, now_ms);
    if migrated > 0 {
        info!("FaceTime state at {}: backfilled creation_time on {} session(s) from an older rustpush", path, migrated);
    }
    match plist::from_value(&raw) {
        Ok(state) => state,
        Err(err) => {
            warn!("Failed to parse FaceTime state at {}: {}", path, err);
            Default::default()
        }
    }
}

/// Add `creation_time` to every stored FaceTime session that lacks it.
/// Returns how many sessions were changed.
fn backfill_facetime_creation_time(raw: &mut plist::Value, now_ms: u64) -> usize {
    let Some(sessions) = raw.as_dictionary_mut()
        .and_then(|root| root.get_mut("sessions"))
        .and_then(|s| s.as_dictionary_mut())
    else {
        return 0;
    };
    let mut migrated = 0usize;
    for session in sessions.values_mut() {
        let Some(session) = session.as_dictionary_mut() else { continue };
        if session.contains_key("creation_time") {
            continue;
        }
        let created = ["last_rekey", "start_time"].iter()
            .find_map(|k| session.get(*k).and_then(plist_value_as_u64))
            .unwrap_or(now_ms);
        session.insert("creation_time".to_string(), plist::Value::Integer(created.into()));
        migrated += 1;
    }
    migrated
}

#[cfg(test)]
mod tests {
    use super::*;
    use rustpush::facetime::{FTMember, FTMessage};
    use std::collections::HashSet;

    fn session(fields: &[(&str, plist::Value)]) -> plist::Value {
        let mut dict = plist::Dictionary::new();
        dict.insert("group_id".into(), plist::Value::String("G".into()));
        for (k, v) in fields {
            dict.insert((*k).into(), v.clone());
        }
        plist::Value::Dictionary(dict)
    }

    fn state_with(sessions: Vec<(&str, plist::Value)>) -> plist::Value {
        let mut map = plist::Dictionary::new();
        for (k, v) in sessions {
            map.insert(k.into(), v);
        }
        let mut root = plist::Dictionary::new();
        root.insert("links".into(), plist::Value::Dictionary(plist::Dictionary::new()));
        root.insert("sessions".into(), plist::Value::Dictionary(map));
        plist::Value::Dictionary(root)
    }

    fn creation_time(raw: &plist::Value, guid: &str) -> Option<u64> {
        raw.as_dictionary()?.get("sessions")?.as_dictionary()?
            .get(guid)?.as_dictionary()?.get("creation_time")?.as_unsigned_integer()
    }

    #[test]
    fn backfills_creation_time_from_rekey_then_start_then_now() {
        let mut raw = state_with(vec![
            ("A", session(&[("last_rekey", plist::Value::Integer(111u64.into())),
                            ("start_time", plist::Value::Integer(222u64.into()))])),
            ("B", session(&[("start_time", plist::Value::Integer(222u64.into()))])),
            ("C", session(&[])),
            ("D", session(&[("creation_time", plist::Value::Integer(5u64.into()))])),
        ]);
        assert_eq!(backfill_facetime_creation_time(&mut raw, 999), 3);
        assert_eq!(creation_time(&raw, "A"), Some(111));
        assert_eq!(creation_time(&raw, "B"), Some(222));
        assert_eq!(creation_time(&raw, "C"), Some(999));
        assert_eq!(creation_time(&raw, "D"), Some(5), "existing value is kept");
    }

    #[test]
    fn tolerates_state_without_sessions() {
        let mut raw = plist::Value::Dictionary(plist::Dictionary::new());
        assert_eq!(backfill_facetime_creation_time(&mut raw, 1), 0);
    }

    #[test]
    fn pre_ad4265a_state_file_loads_after_migration() {
        // Shape of a session persisted by the old pin: no creation_time, and
        // the since-removed is_propped field was never serialized.
        let old = state_with(vec![(
            "3F2504E0-4F89-11D3-9A0C-0305E82C3301",
            session(&[
                ("members", plist::Value::Array(vec![])),
                ("participants", plist::Value::Dictionary(plist::Dictionary::new())),
                ("my_handles", plist::Value::Array(vec![plist::Value::String("tel:+15555550100".into())])),
                ("report_id", plist::Value::String("R".into())),
                ("start_time", plist::Value::Integer(1_700_000_000_000u64.into())),
                ("is_ringing_inaccurate", plist::Value::Boolean(false)),
                ("recent_member_adds", plist::Value::Dictionary(plist::Dictionary::new())),
            ]),
        )]);
        let dir = std::env::temp_dir().join(format!("ft-state-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("facetime-state_test.plist");
        plist::to_file_binary(&path, &old).unwrap();

        let loaded = read_facetime_state(path.to_str().unwrap());
        let session = loaded.sessions.get("3F2504E0-4F89-11D3-9A0C-0305E82C3301")
            .expect("old session must survive the load, not be reset to empty state");
        assert_eq!(session.creation_time, 1_700_000_000_000);
        let _ = std::fs::remove_dir_all(&dir);
    }

    fn leave(handle: &str) -> FTMessage {
        FTMessage::LeaveEvent { guid: "G".into(), participant: 7, handle: handle.into() }
    }

    fn before(ringing: bool, missed: bool) -> Option<SessionBefore> {
        Some(SessionBefore { ringing, missed })
    }

    fn none_mine() -> HashSet<String> {
        HashSet::new()
    }

    #[test]
    fn ring_events_are_ring_notices() {
        assert_eq!(
            notice_for_event(&FTMessage::Ring { guid: "G".into() }, None, false, &none_mine()),
            Some((FACETIME_RING_MARKER, None)),
            "a ring on a session we never saw ring before still announces a call"
        );
        let join = |ring| FTMessage::JoinEvent {
            guid: "G".into(),
            participant: 7,
            handle: "tel:+15555550100".into(),
            ring,
        };
        assert_eq!(
            notice_for_event(&join(true), before(true, false), false, &none_mine()),
            Some((FACETIME_RING_MARKER, Some("tel:+15555550100".into()))),
        );
        assert_eq!(
            notice_for_event(&join(false), before(true, false), false, &none_mine()),
            None,
            "a participant joining a call that is not ringing here is not an incoming call"
        );
        let mut members = HashSet::new();
        members.insert(FTMember { nickname: None, handle: "tel:+15555550123".into() });
        assert_eq!(
            notice_for_event(
                &FTMessage::AddMembers { guid: "G".into(), members, ring: true },
                before(true, false),
                false,
                &none_mine(),
            ),
            Some((FACETIME_RING_MARKER, Some("tel:+15555550123".into()))),
        );
    }

    #[test]
    fn leave_while_still_ringing_is_the_missed_call() {
        // The case this exists for: the caller hung up while we were ringing,
        // so handle() moved the session to FTMode::Missed.
        assert_eq!(
            notice_for_event(
                &leave("tel:+15555550100"),
                before(true, false),
                true,
                &none_mine(),
            ),
            Some((FACETIME_MISSED_MARKER, Some("tel:+15555550100".into()))),
        );
        // A web joiner (temp:<uuid>) is a pseudonym, not a reachable handle.
        assert_eq!(
            notice_for_event(&leave("temp:abc"), before(true, false), true, &none_mine()),
            Some((FACETIME_MISSED_MARKER, None)),
        );
        // Ringing had already stopped, so the LeaveEvent that ended the call
        // already posted the missed notice.
        assert_eq!(
            notice_for_event(&leave("tel:+15555550100"), before(false, false), true, &none_mine()),
            None
        );
        // Someone else left a call still ringing here and the call continues.
        assert_eq!(
            notice_for_event(&leave("tel:+15555550100"), before(true, false), false, &none_mine()),
            None
        );
        // Unknown session: nothing was ringing anywhere, so nothing was missed.
        assert_eq!(
            notice_for_event(&leave("tel:+15555550100"), None, true, &none_mine()),
            None
        );
        // One of our own handles leaving is not a missed call *from* someone.
        let mut my_handles = HashSet::new();
        my_handles.insert("tel:+15555550100".to_string());
        assert_eq!(
            notice_for_event(
                &leave("tel:+15555550100"),
                before(true, false),
                true,
                &my_handles,
            ),
            None,
            "our own handle leaving is not a missed call from someone else"
        );
    }

    #[test]
    fn decline_is_the_only_notice_when_the_call_was_still_ringing() {
        // Unknown session, or still ringing: this Decline is the first notice.
        assert_eq!(
            notice_for_event(&FTMessage::Decline { guid: "G".into() }, None, false, &none_mine()),
            Some((FACETIME_MISSED_MARKER, None)),
        );
        assert_eq!(
            notice_for_event(
                &FTMessage::Decline { guid: "G".into() },
                before(true, false),
                false,
                &none_mine(),
            ),
            Some((FACETIME_MISSED_MARKER, None)),
        );
        // Restored from disk: neither ringing nor missed here, so this Decline
        // is still the first notice.
        assert_eq!(
            notice_for_event(
                &FTMessage::Decline { guid: "G".into() },
                before(false, false),
                false,
                &none_mine(),
            ),
            Some((FACETIME_MISSED_MARKER, None)),
        );
        // Already missed: its LeaveEvent posted the missed notice first.
        assert_eq!(
            notice_for_event(
                &FTMessage::Decline { guid: "G".into() },
                before(false, true),
                false,
                &none_mine(),
            ),
            None,
        );
    }

    #[test]
    fn answered_elsewhere_is_no_notice() {
        // A notify-only bridge never answers, so this would fire for every
        // call picked up on another device.
        assert_eq!(
            notice_for_event(
                &FTMessage::RespondedElsewhere { guid: "G".into() },
                before(true, false),
                true,
                &none_mine(),
            ),
            None,
        );
    }
}
