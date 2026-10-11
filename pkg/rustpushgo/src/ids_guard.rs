// Fault-isolating seam for IDS key lookups.
//
// Every IDS lookup the bridge makes goes through `IdentityManager::cache_keys`,
// which chunks the URI list by 18 and, when Apple answers a chunk with
// web-tunnel status 5206, retries by splitting that chunk in half
// (`ids/user.rs:981-988`). The split computes its chunk size as
// `query.len() / 2`, so a list that is already down to a single URI produces
// `chunks(0)` and panics with "chunk size must be non-zero". uniffi catches
// the panic and the generated Go binding re-panics, taking the bridge down.
//
// That is reachable two ways: a big list halving until it bottoms out on one
// URI, or a list that was one URI to begin with (`cache_keys_once` filters
// participants through `does_not_need_refresh` before chunking, so a warm
// cache routinely leaves exactly one handle to fetch).
//
// This module contains the blast radius WITHOUT changing the shape of IDS
// traffic on the success path — which matters, because Apple rate-limits
// lookups and the StatusKit paths that depend on them are regression-prone:
//
//   * The first attempt is always the caller's full target list, one call,
//     identical to calling `cache_keys` directly. If it succeeds, nothing
//     about the wire behavior differs from before this module existed.
//   * Bisection happens ONLY after a panic. An ordinary `Err` (auth failure,
//     timeout, IDS status) is recorded and does not fan out into more
//     queries — amplifying a 6005 into N lookups would be its own bug.
//   * Handles that panic at size 1 are quarantined in memory so a pass does
//     not re-derive the same poison handle on every cycle.
//
// The quarantine is deliberately NOT persisted. A transient Apple failure
// that got a legitimate handle quarantined across restarts would silently
// degrade presence for that contact indefinitely; an in-memory TTL
// self-heals on restart at a cost of one bisection per boot.
//
// Multi-user: ported from corten-matrix 12447adc, where the quarantine is a
// process-global static. This bridge runs many logins in one process, so the
// quarantine is instead a `HandleQuarantine` value owned by each per-user
// `Client` and passed in by reference. One login's poison handle can never
// suppress another login's lookups, and nothing here is process-global.

use std::collections::HashMap;
use std::future::Future;
use std::panic::AssertUnwindSafe;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use futures::FutureExt;
use log::{debug, error, info, warn};
use rustpush::ids::identity_manager::IdentityManager;
use rustpush::ids::user::QueryOptions;
use rustpush::PushError;

/// How long a handle stays quarantined after panicking as a singleton.
/// In-memory only; cleared on restart.
const QUARANTINE_TTL: Duration = Duration::from_secs(6 * 60 * 60);

/// Bound on total wall time for one guarded lookup, as a multiple of the
/// caller's per-attempt timeout. Bisection can issue several attempts;
/// without a ceiling a pathological list could stall a sync pass.
const BUDGET_MULTIPLIER: u32 = 4;

/// Hard ceiling on `cache_keys` calls per guarded lookup. The wall-clock
/// budget alone is not enough: a panic that fires BEFORE the network call
/// (e.g. `get_main_service`'s `.expect("Topic not found")` for an
/// unregistered sub-service) costs no time, so bisection would run the full
/// 2N-1 attempts instantly.
const MAX_ATTEMPTS: usize = 24;

/// Ceiling on total URIs re-submitted across a bisection, as a multiple of
/// the caller's original list length.
///
/// Attempt count alone does not bound WIRE traffic: each `cache_keys` call
/// chunks by 18 internally, and on a 5206 upstream halves that chunk again
/// (`ids/user.rs:981-988`), so one guard attempt can be several round trips.
/// Counting URIs touched is a much closer proxy for what Apple's rate
/// limiter sees, and it makes the bound scale with the request instead of
/// being a flat constant.
const MAX_TARGET_TOUCHES_MULTIPLIER: usize = 4;

/// More isolated "poison" handles than this in a single lookup means the
/// panic is systemic — a broken registration, a corrupted cache — not a
/// property of any one handle. Quarantining them all would silently disable
/// IDS resolution for the whole address book, so above this threshold the
/// pass quarantines NOTHING and logs loudly instead.
///
/// Known tradeoff of quarantining nothing: a genuinely systemic failure is
/// re-derived on every pass, each one paying a full bisection (bounded by
/// MAX_ATTEMPTS and the URI budget, and doubled by `cache_keys`' own
/// `.retry(max_times(1))` at identity_manager.rs:827). That is bounded and
/// infrequent — the alias pass runs on a 12h cadence plus bootstrap — and is
/// the deliberate price of not blacklisting an entire address book on a
/// failure that is not the handles' fault. If a systemic failure is ever
/// observed in the field, the right follow-up is a short service-level
/// cooldown, NOT lowering this threshold.
const MAX_QUARANTINE_PER_PASS: usize = 2;

/// Whether a call participates in the quarantine.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum QuarantinePolicy {
    /// Skip known-poison handles, and record newly isolated ones.
    Enforce,
    /// Ignore the quarantine entirely — neither read nor write.
    ///
    /// For call sites where suppressing a handle has user-visible
    /// consequences and the quarantine buys no safety: a single-target
    /// lookup that panics is caught, returns empty, and costs one attempt
    /// with or without a quarantine entry.
    Bypass,
}

/// Outcome of a single attempted `cache_keys` call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum AttemptOutcome {
    /// Apple answered; keys (or an empty result) are in the identity cache.
    Ok,
    /// The call panicked. The batch is suspect and gets bisected.
    Panicked(String),
    /// Ordinary failure — IDS error, timeout, transport. NOT bisected.
    Failed(String),
}

/// What a guarded lookup did, for logging and for the caller's own bookkeeping.
#[derive(Debug, Default, PartialEq, Eq)]
pub(crate) struct LookupReport {
    /// Targets covered by an attempt that succeeded.
    pub resolved: Vec<String>,
    /// Targets that panicked when queried alone. Quarantined.
    pub poisoned: Vec<String>,
    /// Targets skipped because they were already quarantined.
    pub skipped: Vec<String>,
    /// First ordinary error seen, if any.
    pub error: Option<String>,
    /// Number of `cache_keys` calls issued. 1 on the happy path.
    pub attempts: usize,
    /// Set when the panic looked systemic rather than handle-specific, in
    /// which case nothing was quarantined.
    pub systemic: bool,
}

impl LookupReport {
    /// Convert an ordinary failure into an `Err`, preserving the `?`
    /// propagation a direct `cache_keys(..).await?` call would have had.
    ///
    /// A caught panic is deliberately NOT an error here: the point of the
    /// seam is that an unanswerable handle leaves the rest of the batch
    /// usable. Only a real lookup failure aborts the caller.
    pub fn into_result(self, context: &str) -> Result<Self, PushError> {
        if let Some(err) = &self.error {
            info!("{context}: {err}");
            // ResourcePanic is rustpush's own carrier for "a managed
            // operation failed with this message" (util.rs:892) and is not
            // special-cased anywhere; DoNotRetry keeps the caller from
            // amplifying a rate-limited failure into a retry storm.
            return Err(PushError::DoNotRetry(Box::new(PushError::ResourcePanic(
                format!("{context}: {err}"),
            ))));
        }
        Ok(self)
    }
}

// ============================================================================
// Quarantine
// ============================================================================

/// Sweep expired entries once the map grows past this. Entries are only added
/// at most `MAX_QUARANTINE_PER_PASS` at a time, so this is a backstop against
/// slow accumulation from handles that are never queried again (which would
/// otherwise never hit the read-path eviction).
const QUARANTINE_SWEEP_AT: usize = 64;

/// Handles whose IDS lookup panicked, for ONE login.
///
/// Owned by the per-user `Client` (never a process-global), so logins in the
/// same process are isolated from each other by construction. Within a login
/// the key still includes the querying handle, because one account can query
/// from several of its own handles.
///
/// Expiry is a monotonic `Instant`, not a wall-clock timestamp: an NTP step
/// or a manual clock change must not stretch (or collapse) a quarantine.
#[derive(Default)]
pub(crate) struct HandleQuarantine {
    entries: Mutex<HashMap<String, Instant>>,
}

impl HandleQuarantine {
    pub(crate) fn new() -> Self {
        Self::default()
    }

    fn key(service: &str, my_handle: &str, target: &str) -> String {
        format!("{service}\u{1}{my_handle}\u{1}{target}")
    }

    /// Record a handle as poison for this service. Expires after
    /// `QUARANTINE_TTL` so a transient failure self-heals.
    pub(crate) fn quarantine(&self, service: &str, my_handle: &str, target: &str) {
        let now = Instant::now();
        let expiry = now + QUARANTINE_TTL;
        if let Ok(mut q) = self.entries.lock() {
            q.insert(Self::key(service, my_handle, target), expiry);
            if q.len() > QUARANTINE_SWEEP_AT {
                q.retain(|_, &mut exp| exp > now);
            }
        }
    }

    /// True if `target` is currently quarantined for `service`. Expired
    /// entries are dropped as a side effect.
    pub(crate) fn is_quarantined(&self, service: &str, my_handle: &str, target: &str) -> bool {
        let key = Self::key(service, my_handle, target);
        let Ok(mut q) = self.entries.lock() else {
            return false;
        };
        match q.get(&key) {
            Some(&expiry) if expiry > Instant::now() => true,
            Some(_) => {
                q.remove(&key);
                false
            }
            None => false,
        }
    }
}

/// Partition `targets` into (queryable, quarantined). Under
/// `QuarantinePolicy::Bypass` nothing is ever skipped.
pub(crate) fn partition_quarantined(
    quarantine: &HandleQuarantine,
    service: &str,
    my_handle: &str,
    targets: &[String],
    policy: QuarantinePolicy,
) -> (Vec<String>, Vec<String>) {
    if policy == QuarantinePolicy::Bypass {
        return (targets.to_vec(), Vec::new());
    }
    let mut kept = Vec::with_capacity(targets.len());
    let mut skipped = Vec::new();
    for t in targets {
        if quarantine.is_quarantined(service, my_handle, t) {
            skipped.push(t.clone());
        } else {
            kept.push(t.clone());
        }
    }
    (kept, skipped)
}

/// Decide what to do with the handles bisection isolated, and record them.
///
/// Returns true when the panic looked systemic. Bisection assumes a panic is
/// caused by the data in the batch; that is false for state-dependent panics
/// (unregistered sub-service, corrupted key cache), where every singleton
/// leaf panics and every handle would otherwise be quarantined at once.
pub(crate) fn classify_and_record(
    quarantine: &HandleQuarantine,
    service: &str,
    my_handle: &str,
    report: &mut LookupReport,
    policy: QuarantinePolicy,
) {
    if report.poisoned.is_empty() {
        return;
    }
    if report.poisoned.len() > MAX_QUARANTINE_PER_PASS {
        report.systemic = true;
        // Surface it as an error too, so callers can tell a systemic failure
        // from a clean pass. What each caller does with it is its own choice:
        // batch_resolve_handles deliberately still runs its in-memory
        // correlation scan on systemic (no queries), while bailing on an
        // ordinary error.
        if report.error.is_none() {
            report.error = Some(format!(
                "systemic {service} lookup failure: {} handle(s) panicked",
                report.poisoned.len()
            ));
        }
        error!(
            "ids_guard: {} lookups panicked for {} of {} handle(s) — treating as a systemic \
             failure (bad registration or cache state), NOT quarantining any handle",
            service,
            report.poisoned.len(),
            report.poisoned.len() + report.resolved.len()
        );
        return;
    }
    for target in &report.poisoned {
        // Naming the handle is the whole diagnostic value of this module and
        // must not depend on the policy: rustpush's own "response too large,
        // chopping in half!" is info-level on a target the bridge's log filter
        // drops, so without this line the handle is unknowable from logs. Emit
        // it under Bypass too — only the recording is policy-dependent.
        if policy == QuarantinePolicy::Bypass {
            info!(
                "ids_guard: {} lookup for {} is unanswerable (upstream panic); \
                 contained, not quarantined (policy bypass at this call site)",
                service, target
            );
            continue;
        }
        quarantine.quarantine(service, my_handle, target);
        info!(
            "ids_guard: {} lookup for {} is unanswerable (upstream panic); \
             quarantining for {}h — every other handle in the batch resolved normally",
            service,
            target,
            QUARANTINE_TTL.as_secs() / 3600
        );
    }
}

/// Remaining time for the next attempt, or None when the budget is spent.
pub(crate) fn next_attempt_timeout(
    deadline: Instant,
    per_attempt: Duration,
    now: Instant,
) -> Option<Duration> {
    let remaining = deadline.saturating_duration_since(now);
    if remaining.is_zero() {
        None
    } else {
        Some(per_attempt.min(remaining))
    }
}


// ============================================================================
// Guarded attempt / bisection
// ============================================================================

/// Run one lookup future under a timeout and a panic guard.
///
/// Generic over the error type so the bisection logic can be unit-tested
/// without an `IdentityManager` or a network.
pub(crate) async fn guarded_attempt<F, T, E>(fut: F, timeout: Duration) -> AttemptOutcome
where
    F: Future<Output = Result<T, E>>,
    E: std::fmt::Debug,
{
    match tokio::time::timeout(timeout, AssertUnwindSafe(fut).catch_unwind()).await {
        Ok(Ok(Ok(_))) => AttemptOutcome::Ok,
        Ok(Ok(Err(e))) => AttemptOutcome::Failed(format!("{e:?}")),
        Ok(Err(panic)) => AttemptOutcome::Panicked(panic_message(&panic)),
        Err(_) => AttemptOutcome::Failed(format!("timed out after {:?}", timeout)),
    }
}

fn panic_message(payload: &Box<dyn std::any::Any + Send>) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        (*s).to_string()
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else {
        "unknown panic".to_string()
    }
}

/// Drive `attempt` over `targets`, bisecting on panic until the offending
/// handles are isolated.
///
/// The first attempt is always the whole list — on the happy path this issues
/// exactly one call with exactly the caller's targets, so IDS traffic is
/// unchanged. Splitting only happens after a panic, which today means an
/// unrecoverable crash.
pub(crate) async fn bisecting_lookup<F, Fut>(targets: Vec<String>, mut attempt: F) -> LookupReport
where
    F: FnMut(Vec<String>) -> Fut,
    Fut: Future<Output = AttemptOutcome>,
{
    let mut report = LookupReport::default();
    if targets.is_empty() {
        return report;
    }

    // Two independent brakes: attempt count (catches instant, pre-network
    // panics) and URIs touched (catches wire amplification, since each
    // attempt can fan out into several round trips inside rustpush).
    let touch_budget = targets.len().saturating_mul(MAX_TARGET_TOUCHES_MULTIPLIER);
    let mut touched = 0usize;

    // Self-check for the invariant this module's whole justification rests on:
    // extra queries may ONLY be issued in response to a panic. Nothing else in
    // this loop pushes work, so `attempts > 1` without a panic is structurally
    // impossible — if it ever happens, an edit has broken the contract and is
    // silently amplifying IDS traffic, which Apple rate-limits. Checked at
    // runtime rather than in a test so a regression reports itself from the
    // field instead of depending on someone running the suite.
    let mut saw_panic = false;

    // Work stack. Halves are pushed right-then-left so the left half is
    // attempted first, keeping the order deterministic in logs.
    let mut work: Vec<Vec<String>> = vec![targets];

    while let Some(batch) = work.pop() {
        if batch.is_empty() {
            continue;
        }
        let over_attempts = report.attempts >= MAX_ATTEMPTS;
        // Always allow the first attempt: the budget must never block the
        // caller's own list from being queried once.
        let over_touches = report.attempts > 0 && touched.saturating_add(batch.len()) > touch_budget;
        if over_attempts || over_touches {
            // Bisection is amplifying without converging. Stop rather than
            // keep hammering IDS; the remaining handles simply go unresolved
            // this pass.
            let abandoned: usize = batch.len() + work.iter().map(|b| b.len()).sum::<usize>();
            if report.error.is_none() {
                let limit = if over_attempts {
                    format!("{MAX_ATTEMPTS}-attempt cap")
                } else {
                    format!("{touch_budget}-uri query budget")
                };
                report.error = Some(format!(
                    "bisection hit the {limit}, {abandoned} handle(s) left unqueried"
                ));
            }
            break;
        }
        report.attempts += 1;
        touched = touched.saturating_add(batch.len());
        match attempt(batch.clone()).await {
            AttemptOutcome::Ok => report.resolved.extend(batch),
            AttemptOutcome::Failed(e) => {
                // Ordinary failure. Do not bisect: that would turn one
                // rate-limited failure into a burst of retries. Record it and
                // keep draining any work that a prior panic already queued.
                if report.error.is_none() {
                    report.error = Some(e);
                }
            }
            AttemptOutcome::Panicked(msg) => {
                saw_panic = true;
                if batch.len() == 1 {
                    report.poisoned.push(batch[0].clone());
                    debug!("ids_guard: isolated poison target {} ({})", batch[0], msg);
                } else {
                    let mid = batch.len() / 2;
                    let right = batch[mid..].to_vec();
                    let left = batch[..mid].to_vec();
                    work.push(right);
                    work.push(left);
                }
            }
        }
    }

    if report.attempts > 1 && !saw_panic {
        warn!(
            "ids_guard: BUG — issued {} lookups with no panic to justify them. \
             Extra IDS queries may only be a response to a contained panic; \
             something in bisecting_lookup is amplifying traffic Apple rate-limits.",
            report.attempts
        );
    }

    report
}

// ============================================================================
// Wiring to rustpush
// ============================================================================

/// Panic- and failure-isolated `IdentityManager::cache_keys`.
///
/// Drop-in for a direct `cache_keys` call: same service, same targets, same
/// flags. On success it issues exactly one query, exactly as before. On a
/// panic it isolates the offending handle(s), quarantines them, and still
/// resolves everything else in the list.
///
/// `per_attempt_timeout_secs` is the caller's existing timeout; total wall
/// time is capped at `BUDGET_MULTIPLIER` times that. `quarantine` is the
/// calling login's own quarantine (see `HandleQuarantine`).
pub(crate) async fn guarded_cache_keys(
    quarantine: &HandleQuarantine,
    identity: &IdentityManager,
    service: &'static str,
    targets: &[String],
    my_handle: &str,
    refresh: bool,
    options: &QueryOptions,
    per_attempt_timeout_secs: u64,
    policy: QuarantinePolicy,
) -> LookupReport {
    let (kept, skipped) = partition_quarantined(quarantine, service, my_handle, targets, policy);
    if !skipped.is_empty() {
        debug!(
            "ids_guard: skipping {} quarantined handle(s) for {}",
            skipped.len(),
            service
        );
    }
    if kept.is_empty() {
        return LookupReport {
            skipped,
            ..Default::default()
        };
    }

    let per_attempt = Duration::from_secs(per_attempt_timeout_secs.max(1));
    let deadline = Instant::now() + per_attempt * BUDGET_MULTIPLIER;
    let handle = my_handle.to_string();
    let required_for_message = options.required_for_message;
    let result_expected = options.result_expected;

    let mut report = bisecting_lookup(kept, |batch| {
        let identity = identity.clone();
        let handle = handle.clone();
        async move {
            let Some(timeout) = next_attempt_timeout(deadline, per_attempt, Instant::now()) else {
                return AttemptOutcome::Failed("guarded lookup budget exhausted".to_string());
            };
            let opts = QueryOptions {
                required_for_message,
                result_expected,
            };
            let fut = identity.cache_keys(service, &batch, &handle, refresh, &opts);
            guarded_attempt(fut, timeout).await
        }
    })
    .await;

    classify_and_record(quarantine, service, my_handle, &mut report, policy);
    if report.attempts > 1 {
        debug!(
            "ids_guard: {} lookup bisected into {} attempts ({} resolved, {} poisoned)",
            service,
            report.attempts,
            report.resolved.len(),
            report.poisoned.len()
        );
    }
    report.skipped = skipped;
    report
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quarantine_is_isolated_per_login() {
        let a = HandleQuarantine::new();
        let b = HandleQuarantine::new();
        a.quarantine("com.apple.madrid", "tel:+1555", "tel:+1666");
        assert!(a.is_quarantined("com.apple.madrid", "tel:+1555", "tel:+1666"));
        assert!(!b.is_quarantined("com.apple.madrid", "tel:+1555", "tel:+1666"));
        assert!(!a.is_quarantined("com.apple.madrid", "tel:+1777", "tel:+1666"));
    }

    #[test]
    fn bypass_never_skips() {
        let q = HandleQuarantine::new();
        q.quarantine("svc", "me", "bad");
        let targets = vec!["bad".to_string(), "good".to_string()];
        let (kept, skipped) = partition_quarantined(&q, "svc", "me", &targets, QuarantinePolicy::Bypass);
        assert_eq!(kept, targets);
        assert!(skipped.is_empty());
        let (kept, skipped) = partition_quarantined(&q, "svc", "me", &targets, QuarantinePolicy::Enforce);
        assert_eq!(kept, vec!["good".to_string()]);
        assert_eq!(skipped, vec!["bad".to_string()]);
    }

    #[tokio::test]
    async fn bisection_isolates_single_poison_handle() {
        let targets: Vec<String> = (0..5).map(|i| format!("h{i}")).collect();
        let report = bisecting_lookup(targets, |batch| async move {
            if batch.iter().any(|t| t == "h3") {
                AttemptOutcome::Panicked("chunk size must be non-zero".into())
            } else {
                AttemptOutcome::Ok
            }
        })
        .await;
        assert_eq!(report.poisoned, vec!["h3".to_string()]);
        assert_eq!(report.resolved.len(), 4);
        assert!(report.error.is_none());
    }

    #[tokio::test]
    async fn ordinary_error_is_not_bisected() {
        let targets: Vec<String> = (0..5).map(|i| format!("h{i}")).collect();
        let report = bisecting_lookup(targets, |_batch| async move {
            AttemptOutcome::Failed("6005".into())
        })
        .await;
        assert_eq!(report.attempts, 1);
        assert!(report.poisoned.is_empty());
        assert_eq!(report.error.as_deref(), Some("6005"));
    }
}
