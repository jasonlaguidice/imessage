// Panic containment for the APNs receive path.
//
// Ported from corten-matrix cd643d57 + e873a23e (with the breadcrumb site
// table from f78fa223 / 8bb06f57). Upstream rustpush signals several routine
// "it isn't there" outcomes with a panic instead of an error (CloudKit's
// `FetchedRecords::get_record` `.expect("No record found?")`, the V1 MMCS
// `unwrap()`s). A panic escaping onto the APNs process task unwinds it, after
// which nothing consumes APNs frames until the Go receive-wedge watchdog
// (`runReceiveWedgeWatchdog` in pkg/connector/client.go) rebuilds the client.
//
// Everything here is either per-connection (the `last_inbound_ms` atomic is
// owned by each user's `WrappedAPSConnection`) or genuinely process-wide (the
// panic hook, which is installed once per process). No per-user state lives
// in a static.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use log::{error, warn};

/// Await `fut`, converting an escaping panic into `None`.
///
/// `what` names the call site, because the panic's own message is very likely
/// suppressed: the panic hook silences panics originating in `icloud/mmcs.rs`
/// and `icloud/cloudkit.rs`, which is where these land.
pub(crate) async fn guard_panic<F: std::future::Future>(what: &str, fut: F) -> Option<F::Output> {
    use futures::FutureExt;
    match std::panic::AssertUnwindSafe(fut).catch_unwind().await {
        Ok(v) => Some(v),
        Err(_) => {
            error!(
                "{} panicked inside upstream rustpush — contained here; continuing without it",
                what
            );
            None
        }
    }
}

/// Backdate the liveness stamp the Go receive-wedge watchdog polls, so its
/// next tick sees a wedge and rebuilds this user's client.
///
/// `WrappedAPSConnection::seconds_since_last_inbound` reports `now - stamp`,
/// and `runReceiveWedgeWatchdog` rebuilds once that crosses
/// `receiveWedgeRecoverySecs`. When nothing is consuming APNs frames any more
/// the stamp would reach the threshold on its own, but only after the whole
/// threshold elapsed with inbound messages hitting the floor. Backdating
/// collapses that to the watchdog's next 60s tick.
///
/// One hour is deliberately far past any plausible value of that constant
/// (600s today) so the two stay decoupled, while still producing a readable
/// `idle_secs` in the watchdog's log line.
pub(crate) fn force_receive_wedge(last_inbound_ms: &AtomicU64) {
    const FORCE_WEDGE_BACKDATE_MS: u64 = 60 * 60 * 1000;
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;
    last_inbound_ms.store(now_ms.saturating_sub(FORCE_WEDGE_BACKDATE_MS), Ordering::Relaxed);
}

/// Held by the APNs process task for its entire life. Its `Drop` fires on
/// every exit, but only *acts* when the task is unwinding on a panic:
/// `thread::panicking()` is false on a normal return and on the `abort()` that
/// teardown uses, so a clean shutdown stays silent.
///
/// NOTE: this backdate alone is not sufficient. The drain task stamps
/// `last_inbound_ms` on every frame BEFORE it discovers the receiver is gone,
/// so the first frame after this fires (a keepalive Pong, within ~60s)
/// overwrites it. `force_receive_wedge` is therefore applied AGAIN at that
/// detection point in the drain, which is the write that actually survives.
/// This one covers the window before the drain notices.
pub(crate) struct ProcessTaskUnwindSentinel {
    pub(crate) last_inbound_ms: Arc<AtomicU64>,
}

impl Drop for ProcessTaskUnwindSentinel {
    fn drop(&mut self) {
        if !std::thread::panicking() {
            return;
        }
        error!(
            "APNs process task is UNWINDING ON A PANIC — inbound message processing has stopped \
             for this connection. Forcing the receive-wedge watchdog to rebuild the client on its \
             next tick instead of waiting out the full idle threshold."
        );
        force_receive_wedge(&self.last_inbound_ms);
    }
}

/// Record a panic the hook is suppressing, leaving a rate-limited breadcrumb.
///
/// The hook filters by FILE, not by "is anyone actually catching this", so
/// without a breadcrumb it erased UNCAUGHT panics from these files as
/// thoroughly as the expected Ford-key brute-force ones. That is how an
/// unguarded `expect("No record found?")` could unwind the APNs process task
/// with zero trace in the logs.
///
/// Two outputs:
///   * a summary once per window: how many were swallowed and the latest site;
///   * an immediate line the first time a site is seen in a window, since a
///     new site is the interesting case. A small table of reported sites keeps
///     an alternating flood from spending that budget on known sites.
///
/// Relaxed atomics only: a lock here could be held by the panicking thread,
/// and a panic inside a panic hook aborts the process. The scan-then-claim is
/// deliberately not atomic; two threads panicking at the same NEW site can
/// both print (bounded to two lines, cannot exhaust the table).
pub(crate) fn record_suppressed_panic(file: &str, line: u32) {
    const BREADCRUMB_INTERVAL_SECS: u64 = 60;
    const REPORTED_SLOTS: usize = 8;
    static LAST_BREADCRUMB_SECS: AtomicU64 = AtomicU64::new(0);
    static SUPPRESSED_COUNT: AtomicU64 = AtomicU64::new(0);
    #[allow(clippy::declare_interior_mutable_const)]
    const EMPTY_SLOT: AtomicU64 = AtomicU64::new(0);
    static REPORTED_SITES: [AtomicU64; REPORTED_SLOTS] = [EMPTY_SLOT; REPORTED_SLOTS];
    static REPORTED_CURSOR: AtomicU64 = AtomicU64::new(0);

    SUPPRESSED_COUNT.fetch_add(1, Ordering::Relaxed);
    let site = site_hash(file, line);
    let now_secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    let last = LAST_BREADCRUMB_SECS.load(Ordering::Relaxed);
    if now_secs.saturating_sub(last) >= BREADCRUMB_INTERVAL_SECS
        && LAST_BREADCRUMB_SECS
            .compare_exchange(last, now_secs, Ordering::Relaxed, Ordering::Relaxed)
            .is_ok()
    {
        // New window: forget reported sites and seed the table with this one
        // so the summary is not immediately echoed by a novel-site line.
        for slot in REPORTED_SITES.iter() {
            slot.store(0, Ordering::Relaxed);
        }
        REPORTED_SITES[0].store(site, Ordering::Relaxed);
        REPORTED_CURSOR.store(1, Ordering::Relaxed);
        let swallowed = SUPPRESSED_COUNT.swap(0, Ordering::Relaxed);
        warn!(
            "suppressed {} rustpush MMCS/CloudKit panic(s) since the last breadcrumb; \
             most recent at {}:{} (this summary is rate-limited to one line per {}s). \
             These are EXPECTED for the catch_unwind-guarded recovery paths. If one \
             shows up next to a receive stall or a 'process task' line, an UNGUARDED \
             call site is unwinding its task.",
            swallowed, file, line, BREADCRUMB_INTERVAL_SECS
        );
    } else if !REPORTED_SITES
        .iter()
        .any(|slot| slot.load(Ordering::Relaxed) == site)
    {
        let cursor = REPORTED_CURSOR.fetch_add(1, Ordering::Relaxed);
        if (cursor as usize) < REPORTED_SLOTS {
            REPORTED_SITES[cursor as usize].store(site, Ordering::Relaxed);
            // Printed on its own, so take it back out of the summary count.
            let _ = SUPPRESSED_COUNT.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| {
                Some(n.saturating_sub(1))
            });
            warn!(
                "suppressed rustpush panic at a site not yet reported this window: \
                 {}:{}. Printed immediately rather than waiting for the {}s summary, \
                 because a new site is the interesting case — check whether this call \
                 site is actually inside a catch_unwind. At most {} distinct sites are \
                 reported per window.",
                file, line, BREADCRUMB_INTERVAL_SECS, REPORTED_SLOTS
            );
        }
    }
}

/// FNV-1a over file+line. Inline and allocation-free: this runs inside a
/// panic hook, where a panic of our own would abort.
fn site_hash(file: &str, line: u32) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in file.as_bytes() {
        h ^= *b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h ^= line as u64;
    h.wrapping_mul(0x0000_0100_0000_01b3)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn force_receive_wedge_backdates_past_watchdog_threshold() {
        let stamp = AtomicU64::new(0);
        force_receive_wedge(&stamp);
        let now_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64;
        let idle_secs = (now_ms - stamp.load(Ordering::Relaxed)) / 1000;
        assert!(idle_secs >= 600, "idle_secs={idle_secs}");
    }

    #[test]
    fn site_hash_distinguishes_lines() {
        assert_ne!(site_hash("icloud/mmcs.rs", 1), site_hash("icloud/mmcs.rs", 2));
        assert_eq!(site_hash("icloud/mmcs.rs", 1), site_hash("icloud/mmcs.rs", 1));
    }

    #[tokio::test]
    async fn guard_panic_contains_panics() {
        assert_eq!(guard_panic("ok", async { 7 }).await, Some(7));
        let r: Option<()> = guard_panic("boom", async { panic!("No record found?") }).await;
        assert!(r.is_none());
    }
}
