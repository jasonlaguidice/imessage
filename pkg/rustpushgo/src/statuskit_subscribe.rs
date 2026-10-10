//! Pure planning helpers for the StatusKit presence subscribe (no I/O).
//!
//! Both helpers are decision-only: they read the maps/counts the caller hands
//! them and return a plan. All file parsing (statuskit-state.plist /
//! statuskit-channel-dates.plist), state mutation and logging stay in
//! `Client::subscribe_to_status` in lib.rs, which keeps this module testable
//! without touching a real StatusKit client.

use std::collections::{HashMap, HashSet};

/// Max StatusKit channels per SubscribeToChannels batch (Apple bounces larger
/// ones). Shared by the over-budget prune and the chunker.
pub(crate) const CHANNEL_BUDGET: usize = 15;

/// One over-budget "from" handle the prune capped: (handle, total channels, kept).
/// Only used to build the caller's log line.
pub(crate) struct CappedHandle {
    pub from: String,
    pub total: usize,
    pub kept: usize,
}

/// What the prune decided: the channel ids to drop from the StatusKit state,
/// plus one entry per handle it had to cap.
pub(crate) struct PrunePlan {
    pub prune: HashSet<String>,
    pub capped: Vec<CappedHandle>,
}

/// Decide which channels to drop so no "from" handle keeps more than `budget`
/// channels. Keeps every live channel (chan_live true) up to the budget, then
/// fills the rest with dead placeholders: most-recent invite date first when any
/// placeholder has a date > 0 (undated placeholders — those that arrived by APNs
/// reshare and have not been seen in CloudKit yet — rank as *newest* and so are
/// kept ahead of every dated one), otherwise a window rotated by `rot`.
pub(crate) fn plan_channel_prune(
    chan_by_from: &HashMap<String, Vec<String>>,
    chan_live: &HashMap<String, bool>,
    chan_date: &HashMap<String, i64>,
    rot: usize,
    budget: usize,
) -> PrunePlan {
    let mut prune: HashSet<String> = HashSet::new();
    let mut capped: Vec<CappedHandle> = Vec::new();
    for (from, cids) in chan_by_from {
        if cids.len() <= budget {
            continue;
        }
        let mut sorted = cids.clone();
        sorted.sort();
        let mut keep: HashSet<String> = sorted
            .iter()
            .filter(|c| *chan_live.get(*c).unwrap_or(&false))
            .take(budget)
            .cloned()
            .collect();
        let mut placeholders: Vec<&String> = sorted
            .iter()
            .filter(|c| !*chan_live.get(*c).unwrap_or(&false))
            .collect();
        let remaining = budget.saturating_sub(keep.len());
        if remaining > 0 && !placeholders.is_empty() {
            // Prefer the most-recently-invited channels (current first) when we
            // have dates; otherwise rotate the window. A channel with no date
            // ranks as *newest*: after a dated CloudKit pull every CloudKit
            // channel has a date, so an undated one is a key that arrived by
            // APNs reshare — likely the peer's current channel.
            let have_dates = placeholders
                .iter()
                .any(|c| chan_date.get(*c).copied().unwrap_or(0) > 0);
            if have_dates {
                let rank = |c: &String| match chan_date.get(c).copied().unwrap_or(0) {
                    0 => i64::MAX,
                    d => d,
                };
                placeholders.sort_by(|a, b| rank(*b).cmp(&rank(*a)));
                for c in placeholders.iter().take(remaining) {
                    keep.insert((*c).clone());
                }
            } else {
                let start = rot.wrapping_mul(remaining) % placeholders.len();
                for i in 0..remaining.min(placeholders.len()) {
                    keep.insert(placeholders[(start + i) % placeholders.len()].clone());
                }
            }
        }
        for c in cids {
            if !keep.contains(c) {
                prune.insert(c.clone());
            }
        }
        capped.push(CappedHandle {
            from: from.clone(),
            total: cids.len(),
            kept: keep.len(),
        });
    }
    PrunePlan { prune, capped }
}

/// Greedily bin-pack handles into chunks whose summed channel counts stay
/// within `budget`. A single handle over budget forms its own chunk.
/// Input order is preserved, so callers control determinism.
pub(crate) fn chunk_handles_by_channel_budget(
    counts: Vec<(String, usize)>,
    budget: usize,
) -> Vec<Vec<String>> {
    let mut chunks: Vec<Vec<String>> = Vec::new();
    let mut cur: Vec<String> = Vec::new();
    let mut cur_count = 0usize;
    for (handle, count) in counts {
        if cur_count + count > budget && !cur.is_empty() {
            chunks.push(std::mem::take(&mut cur));
            cur_count = 0;
        }
        cur.push(handle);
        cur_count += count;
    }
    if !cur.is_empty() {
        chunks.push(cur);
    }
    chunks
}

#[cfg(test)]
mod tests {
    use super::*;

    fn handle() -> String {
        "tel:+15550001111".to_string()
    }

    /// 20 lexicographically sorted channel ids: "c00".."c19".
    fn channels(n: usize) -> Vec<String> {
        (0..n).map(|i| format!("c{i:02}")).collect()
    }

    /// Everything NOT pruned for the (single) handle in the plan.
    fn kept_set(cids: &[String], plan: &PrunePlan) -> HashSet<String> {
        cids.iter()
            .filter(|c| !plan.prune.contains(*c))
            .cloned()
            .collect()
    }

    #[test]
    fn under_budget_handle_is_left_alone() {
        let cids = channels(12);
        let mut by_from = HashMap::new();
        by_from.insert(handle(), cids);
        let plan = plan_channel_prune(
            &by_from,
            &HashMap::new(),
            &HashMap::new(),
            0,
            CHANNEL_BUDGET,
        );
        assert!(plan.prune.is_empty(), "nothing may be pruned: {:?}", plan.prune);
        assert!(plan.capped.is_empty());
    }

    #[test]
    fn keeps_every_live_channel_and_pads_to_budget() {
        // 20 channels, the first 3 live, no invite dates at all.
        let cids = channels(20);
        let mut live = HashMap::new();
        for c in cids.iter().take(3) {
            live.insert(c.clone(), true);
        }
        let mut by_from = HashMap::new();
        by_from.insert(handle(), cids.clone());
        let plan = plan_channel_prune(&by_from, &live, &HashMap::new(), 0, CHANNEL_BUDGET);

        assert_eq!(plan.prune.len(), 5, "20 - 15 kept");
        for c in cids.iter().take(3) {
            assert!(!plan.prune.contains(c), "live channel {} was pruned", c);
        }
        assert_eq!(plan.capped.len(), 1);
        assert_eq!(plan.capped[0].total, 20);
        assert_eq!(plan.capped[0].kept, CHANNEL_BUDGET);
    }

    #[test]
    fn dated_placeholders_keep_the_most_recent() {
        // 20 dead placeholders, dated c00 (oldest) .. c19 (newest): the 15
        // newest dates win, so c00..c04 are the ones dropped.
        let cids = channels(20);
        let dates: HashMap<String, i64> = cids
            .iter()
            .enumerate()
            .map(|(i, c)| (c.clone(), 1_700_000_000 + i as i64))
            .collect();
        let mut by_from = HashMap::new();
        by_from.insert(handle(), cids.clone());
        let plan = plan_channel_prune(&by_from, &HashMap::new(), &dates, 0, CHANNEL_BUDGET);

        assert_eq!(plan.prune.len(), 5);
        for c in cids.iter().take(5) {
            assert!(plan.prune.contains(c), "oldest channel {} was kept", c);
        }
        for c in cids.iter().skip(5) {
            assert!(!plan.prune.contains(c), "newer channel {} was pruned", c);
        }
        assert_eq!(plan.capped[0].kept, CHANNEL_BUDGET);
    }

    #[test]
    fn undated_placeholder_ranks_newest() {
        // 20 dead placeholders: c00..c18 carry ascending invite dates, c19 has
        // no date at all (it arrived by APNs reshare, so it is likely the
        // peer's CURRENT channel). Undated ranks as newest, so c19 is kept and
        // the five oldest dated channels (c00..c04) are the ones dropped.
        let cids = channels(20);
        let dates: HashMap<String, i64> = cids
            .iter()
            .take(19)
            .enumerate()
            .map(|(i, c)| (c.clone(), 1_700_000_000 + i as i64))
            .collect();
        assert!(!dates.contains_key("c19"), "c19 must stay undated");
        let mut by_from = HashMap::new();
        by_from.insert(handle(), cids.clone());
        let plan = plan_channel_prune(&by_from, &HashMap::new(), &dates, 0, CHANNEL_BUDGET);

        let kept = kept_set(&cids, &plan);
        assert_eq!(plan.prune.len(), 5, "15 of 20 kept: {:?}", plan.prune);
        assert!(
            !plan.prune.contains("c19"),
            "undated channel c19 (APNs-delivered) must rank newest and be kept"
        );
        assert!(kept.contains("c19"));
        for c in ["c00", "c01", "c02", "c03", "c04"] {
            assert!(
                plan.prune.contains(c),
                "oldest dated channel {} should have been pruned",
                c
            );
        }
        for c in cids.iter().skip(5).take(14) {
            assert!(
                !plan.prune.contains(c),
                "newer dated channel {} should have been kept",
                c
            );
        }
        assert_eq!(kept.len(), CHANNEL_BUDGET);
        assert_eq!(plan.capped[0].kept, CHANNEL_BUDGET);
    }

    #[test]
    fn rotating_window_advances_with_rot() {
        // No dates at all: without a rotation cursor the same 15 placeholders
        // would be subscribed forever, so the window has to move with rot.
        let cids = channels(20);
        let mut by_from = HashMap::new();
        by_from.insert(handle(), cids.clone());
        let plan0 = plan_channel_prune(&by_from, &HashMap::new(), &HashMap::new(), 0, CHANNEL_BUDGET);
        let plan1 = plan_channel_prune(&by_from, &HashMap::new(), &HashMap::new(), 1, CHANNEL_BUDGET);

        let kept0 = kept_set(&cids, &plan0);
        let kept1 = kept_set(&cids, &plan1);
        assert_eq!(kept0.len(), CHANNEL_BUDGET);
        assert_eq!(kept1.len(), CHANNEL_BUDGET);
        assert_ne!(kept0, kept1, "rot must move the placeholder window");
    }

    #[test]
    fn chunks_stay_within_budget() {
        let counts = vec![
            ("a".to_string(), 10usize),
            ("b".to_string(), 10),
            ("c".to_string(), 3),
        ];
        assert_eq!(
            chunk_handles_by_channel_budget(counts, 15),
            vec![vec!["a".to_string()], vec!["b".to_string(), "c".to_string()]]
        );
    }

    #[test]
    fn over_budget_handle_forms_its_own_chunk() {
        let counts = vec![("big".to_string(), 40usize), ("b".to_string(), 10)];
        assert_eq!(
            chunk_handles_by_channel_budget(counts, 15),
            vec![vec!["big".to_string()], vec!["b".to_string()]]
        );
    }

    #[test]
    fn zero_count_handles_pack_together() {
        // Ghost handles without a key are "free" in the budget, so they share
        // one chunk (and one SubscribeToChannels).
        let counts = vec![
            ("x".to_string(), 0usize),
            ("y".to_string(), 0),
            ("z".to_string(), 0),
        ];
        assert_eq!(
            chunk_handles_by_channel_budget(counts, 15),
            vec![vec!["x".to_string(), "y".to_string(), "z".to_string()]]
        );
        assert!(chunk_handles_by_channel_budget(Vec::new(), 15).is_empty());
    }
}
