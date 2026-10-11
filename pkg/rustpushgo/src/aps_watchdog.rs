// In-process APNs recovery: the Rust half of the receive watchdog.
//
// Ported from corten-matrix 0f8623b4 (half-open stall -> request_update),
// 2e0791b4 (TCP keep-alive + write timeout on the courier socket) and
// 4a40f6ac (1h "live but deaf" detection), in their upstream/master form.
//
// Three layers, cheapest first:
//   1. Kernel: TCP keep-alive + a write/user timeout on the APNs courier
//      socket make a half-open connection ERROR within ~90-150s instead of
//      blocking read_exact()/write_all() for the kernel default or forever.
//      rustpush's ResourceManager then regenerates the socket.
//   2. `ApnsLivenessWatchdog` (driven by each connection's drain task):
//      forces a TRANSPORT-ONLY reconnect via `request_update()` after 300s
//      with no inbound frame at all, or after 1h of frames but no IDS
//      notification. No IDS re-registration.
//   3. Go: `runReceiveWedgeWatchdog` escalates to a full client rebuild after
//      600s of true silence (see `WrappedAPSConnection::last_inbound_ms`).
//
// Multi-user: the watchdog state is owned by each connection's drain task.
// The keep-alive sweep is process-wide by design: it hardens every APNs
// courier socket in the process (one per login) and holds no per-user state.

use std::time::Duration;

use std::time::Instant;

use log::info;

/// No inbound frame (not even the ~60s keepalive Pong) for this long means
/// the receive path has silently wedged, e.g. a half-open courier socket.
/// Five missed keepalive cycles, so a healthy-but-quiet link never trips it.
pub(crate) const APNS_STALL_TIMEOUT: Duration = Duration::from_secs(300);

/// Transport alive (Pongs, our own send-Acks) but no IDS notification for this
/// long: Apple may have silently stopped routing madrid pushes to this token
/// ("I can send but stopped receiving"). Deliberately long, since a genuinely
/// idle link looks the same; at worst that link sees one harmless reconnect
/// per hour.
pub(crate) const APNS_RECEIVE_STALL_TIMEOUT: Duration = Duration::from_secs(3600);

/// How often the drain task polls the watchdog.
pub(crate) const APNS_WATCHDOG_TICK: Duration = Duration::from_secs(30);

/// What the watchdog wants the drain task to do on a tick.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum StallVerdict {
    Healthy,
    /// No inbound frame at all for `idle`.
    TransportStalled { idle: Duration },
    /// Frames arrive, but no IDS notification for `deaf`.
    RoutingDeaf { idle: Duration, deaf: Duration },
}

/// Per-connection liveness bookkeeping for the APNs drain task.
///
/// Deliberately separate from `WrappedAPSConnection::last_inbound_ms` (the
/// Go watchdog's signal): after a forced reconnect this resets its own timers
/// to space reconnects out, but must NOT reset the Go-visible stamp, so the
/// Go side can still measure true silence and escalate when `request_update`
/// keeps failing.
///
/// Time is passed in (monotonic `Instant`) so the policy is testable without
/// a runtime clock.
pub(crate) struct ApnsLivenessWatchdog {
    last_inbound: Instant,
    last_notification: Instant,
}

impl ApnsLivenessWatchdog {
    pub(crate) fn new(now: Instant) -> Self {
        Self {
            last_inbound: now,
            last_notification: now,
        }
    }

    /// Any inbound frame proves the transport is alive; a Notification frame
    /// additionally proves Apple's push routing to this token is alive. Pongs
    /// and our own send-Acks do NOT: they are what masks a live-but-deaf link.
    pub(crate) fn on_frame(&mut self, now: Instant, is_notification: bool) {
        self.last_inbound = now;
        if is_notification {
            self.last_notification = now;
        }
    }

    /// The broadcast receiver lagged: frames were flowing fast enough to fall
    /// behind, almost certainly including real notifications, so refresh both
    /// timers to avoid a spurious re-filter during a genuine burst.
    pub(crate) fn on_lagged(&mut self, now: Instant) {
        self.on_frame(now, true);
    }

    /// Decide whether to force a reconnect, and if so reset the timers that
    /// verdict consumes so forced reconnects are spaced apart.
    pub(crate) fn check(&mut self, now: Instant) -> StallVerdict {
        let idle = now.saturating_duration_since(self.last_inbound);
        if idle >= APNS_STALL_TIMEOUT {
            // The regenerated link starts clean; also reset the notification
            // timer so the deaf branch does not fire on the pre-reconnect gap.
            self.last_inbound = now;
            self.last_notification = now;
            return StallVerdict::TransportStalled { idle };
        }
        let deaf = now.saturating_duration_since(self.last_notification);
        if deaf >= APNS_RECEIVE_STALL_TIMEOUT {
            // Reset only the notification timer; the reconnect refreshes
            // last_inbound on its own.
            self.last_notification = now;
            return StallVerdict::RoutingDeaf { idle, deaf };
        }
        StallVerdict::Healthy
    }
}

// ---------------------------------------------------------------------------
// Courier-socket TCP keep-alive
// ---------------------------------------------------------------------------
// rustpush opens the courier socket itself and exposes no handle, so we find
// it by peer port (5223 is APNs-courier-specific; homeserver, CardDAV and IDS
// sockets are all :443) and set the options on the fd. Idempotent and
// best-effort, re-applied on a timer so it covers the initial connect and
// every reconnect; failures are ignored.

const APNS_COURIER_PORT: u16 = 5223;

static APNS_KEEPALIVE_TASK_STARTED: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

#[cfg(any(target_os = "linux", target_os = "macos"))]
fn aps_list_open_fds() -> Vec<i32> {
    // Linux: exact open-fd set via /proc/self/fd. macOS (no procfs): bounded
    // scan up to the soft NOFILE limit; getpeername on a closed fd is an
    // immediate EBADF, so this is just fast syscalls.
    if let Ok(rd) = std::fs::read_dir("/proc/self/fd") {
        let mut v = Vec::new();
        for e in rd.flatten() {
            if let Ok(n) = e.file_name().to_string_lossy().parse::<i32>() {
                v.push(n);
            }
        }
        if !v.is_empty() {
            return v;
        }
    }
    let mut cap: i64 = 4096;
    unsafe {
        let mut lim: libc::rlimit = std::mem::zeroed();
        if libc::getrlimit(libc::RLIMIT_NOFILE, &mut lim) == 0 {
            // Do not clamp below NOFILE or a high courier fd can be missed.
            cap = (lim.rlim_cur as i64).clamp(1024, 65536);
        }
    }
    (0..cap as i32).collect()
}

#[cfg(any(target_os = "linux", target_os = "macos"))]
fn aps_socket_peer_port(fd: i32) -> Option<u16> {
    unsafe {
        let mut sotype: libc::c_int = 0;
        let mut tlen = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
        if libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_TYPE,
            &mut sotype as *mut _ as *mut libc::c_void,
            &mut tlen,
        ) != 0
        {
            return None;
        }
        if sotype != libc::SOCK_STREAM {
            return None;
        }
        let mut addr: libc::sockaddr_storage = std::mem::zeroed();
        let mut len = std::mem::size_of::<libc::sockaddr_storage>() as libc::socklen_t;
        if libc::getpeername(fd, &mut addr as *mut _ as *mut libc::sockaddr, &mut len) != 0 {
            return None;
        }
        match addr.ss_family as libc::c_int {
            libc::AF_INET => {
                let a = &*(&addr as *const _ as *const libc::sockaddr_in);
                Some(u16::from_be(a.sin_port))
            }
            libc::AF_INET6 => {
                let a = &*(&addr as *const _ as *const libc::sockaddr_in6);
                Some(u16::from_be(a.sin6_port))
            }
            _ => None,
        }
    }
}

/// Harden one courier socket. Returns true only when it was NEWLY hardened.
#[cfg(any(target_os = "linux", target_os = "macos"))]
fn aps_apply_keepalive(fd: i32) -> bool {
    unsafe fn setopt(fd: i32, level: libc::c_int, opt: libc::c_int, val: libc::c_int) {
        let _ = libc::setsockopt(
            fd,
            level,
            opt,
            &val as *const libc::c_int as *const libc::c_void,
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        );
    }
    unsafe {
        // One-shot per socket epoch: a fresh rustpush courier socket has
        // SO_KEEPALIVE off; once on, skip it on later sweeps. Re-arming every
        // sweep could keep resetting the idle countdown.
        let mut already: libc::c_int = 0;
        let mut alen = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
        if libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_KEEPALIVE,
            &mut already as *mut _ as *mut libc::c_void,
            &mut alen,
        ) == 0
            && already != 0
        {
            return false;
        }
        let on: libc::c_int = 1;
        let ok = libc::setsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_KEEPALIVE,
            &on as *const libc::c_int as *const libc::c_void,
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        ) == 0;
        let tcp = libc::IPPROTO_TCP;
        // 90s idle is ABOVE rustpush's 60s app-level Ping cadence, so on a
        // healthy link app traffic always resets the timer first and the
        // kernel never probes: no extra traffic to Apple.
        #[cfg(target_os = "linux")]
        {
            // Stable kernel ABI values (netinet/tcp.h).
            const TCP_KEEPIDLE: libc::c_int = 4;
            const TCP_KEEPINTVL: libc::c_int = 5;
            const TCP_KEEPCNT: libc::c_int = 6;
            const TCP_USER_TIMEOUT: libc::c_int = 18;
            setopt(fd, tcp, TCP_KEEPIDLE, 90);
            setopt(fd, tcp, TCP_KEEPINTVL, 15);
            setopt(fd, tcp, TCP_KEEPCNT, 4);
            // Bound unacked (wedged) writes: write_all() errors in ~90s,
            // freeing rustpush's socket lock.
            setopt(fd, tcp, TCP_USER_TIMEOUT, 90_000);
        }
        #[cfg(target_os = "macos")]
        {
            // Stable values from <netinet/tcp.h>.
            const TCP_KEEPALIVE: libc::c_int = 0x10;
            const TCP_KEEPINTVL: libc::c_int = 0x101;
            const TCP_KEEPCNT: libc::c_int = 0x102;
            const TCP_RXT_CONNDROPTIME: libc::c_int = 0x80;
            setopt(fd, tcp, TCP_KEEPALIVE, 90);
            setopt(fd, tcp, TCP_KEEPINTVL, 15);
            setopt(fd, tcp, TCP_KEEPCNT, 4);
            setopt(fd, tcp, TCP_RXT_CONNDROPTIME, 90);
        }
        ok
    }
}

#[cfg(any(target_os = "linux", target_os = "macos"))]
fn set_apns_socket_keepalive() {
    let mut hardened = 0usize;
    for fd in aps_list_open_fds() {
        if aps_socket_peer_port(fd) == Some(APNS_COURIER_PORT) && aps_apply_keepalive(fd) {
            hardened += 1;
        }
    }
    if hardened > 0 {
        // Only logged when a socket is NEWLY hardened (per connect/reconnect).
        info!(
            "APNs keepalive: applied TCP keep-alive + write timeout to {} courier socket(s) (:{}); a half-open courier socket now self-recovers in ~90-150s instead of stalling indefinitely",
            hardened, APNS_COURIER_PORT
        );
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
fn set_apns_socket_keepalive() {}

/// Spawn (once per process) the sweep that keeps TCP keep-alive + a write
/// timeout applied to every APNs courier socket. Must be called from within
/// the tokio runtime.
pub(crate) fn ensure_apns_keepalive_task() {
    use std::sync::atomic::Ordering;
    if APNS_KEEPALIVE_TASK_STARTED.swap(true, Ordering::SeqCst) {
        return;
    }
    tokio::spawn(async move {
        loop {
            set_apns_socket_keepalive();
            tokio::time::sleep(Duration::from_secs(45)).await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn healthy_link_is_left_alone() {
        let t0 = Instant::now();
        let mut w = ApnsLivenessWatchdog::new(t0);
        let t1 = t0 + Duration::from_secs(60);
        w.on_frame(t1, false);
        assert_eq!(w.check(t1 + Duration::from_secs(30)), StallVerdict::Healthy);
    }

    #[test]
    fn half_open_stall_fires_once_then_spaces_out() {
        let t0 = Instant::now();
        let mut w = ApnsLivenessWatchdog::new(t0);
        let t1 = t0 + APNS_STALL_TIMEOUT;
        assert!(matches!(w.check(t1), StallVerdict::TransportStalled { .. }));
        assert_eq!(w.check(t1 + APNS_WATCHDOG_TICK), StallVerdict::Healthy);
    }

    #[test]
    fn live_but_deaf_fires_after_an_hour_of_pongs() {
        let t0 = Instant::now();
        let mut w = ApnsLivenessWatchdog::new(t0);
        let step = Duration::from_secs(60);
        let mut now = t0;
        while now + step < t0 + APNS_RECEIVE_STALL_TIMEOUT {
            now += step;
            w.on_frame(now, false);
            assert_eq!(w.check(now), StallVerdict::Healthy);
        }
        now += step;
        w.on_frame(now, false);
        assert!(matches!(w.check(now), StallVerdict::RoutingDeaf { .. }));
        assert_eq!(w.check(now), StallVerdict::Healthy);
    }

    #[test]
    fn notification_resets_deaf_timer() {
        let t0 = Instant::now();
        let mut w = ApnsLivenessWatchdog::new(t0);
        let t1 = t0 + APNS_RECEIVE_STALL_TIMEOUT - Duration::from_secs(1);
        w.on_frame(t1, true);
        assert_eq!(w.check(t1 + Duration::from_secs(2)), StallVerdict::Healthy);
    }
}
