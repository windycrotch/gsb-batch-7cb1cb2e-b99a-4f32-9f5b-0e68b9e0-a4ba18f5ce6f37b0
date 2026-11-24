use std::fmt::Debug;
use std::io;
use std::ops::Range;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering};
use std::time::Instant;

use parking_lot::{Condvar, Mutex, RwLock};
use rangemap::RangeSet;
use tokio::sync::{Notify, mpsc};
use tracing::debug;

/// Observable terminal state of the download task.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum DownloadTermination {
    /// The download finished naturally. Successfully downloaded data remains readable.
    Completed,
    /// The download failed.
    Failed,
    /// The download was cancelled by the user or when the reader was dropped.
    Cancelled,
}

impl DownloadTermination {
    pub(crate) fn result(self) -> io::Result<()> {
        match self {
            Self::Completed => Ok(()),
            Self::Failed => Err(io::Error::other("stream failed to download")),
            // `Interrupted` would make `read_to_end`/`read_exact` retry a permanent cancellation.
            // Expose this as a non-retryable terminal error to blocking callers.
            Self::Cancelled => Err(io::Error::other("stream download cancelled")),
        }
    }
}

#[derive(Debug, Clone)]
pub(crate) struct SourceHandle {
    pub(super) downloaded: Downloaded,
    pub(super) download_status: DownloadStatus,
    pub(super) requested_position: RequestedPosition,
    pub(super) position_reached: PositionReached,
    pub(super) content_length: Option<u64>,
    pub(super) seek_tx: mpsc::UnboundedSender<u64>,
    pub(super) notify_read: NotifyRead,
}

impl SourceHandle {
    pub(crate) fn get_downloaded_at_position(&self, position: u64) -> Option<Range<u64>> {
        self.downloaded.get(position)
    }

    /// Waits until the range containing `start` extends through `requested_end`.
    ///
    /// The predicate is rechecked after every wake-up, so concurrent requests cannot consume a
    /// single shared notification or clear a newer request.
    pub(crate) fn wait_for_range(&self, start: u64, requested_end: u64) -> io::Result<()> {
        self.request_position(requested_end);
        debug!(
            start,
            requested_position = requested_end,
            "waiting for requested range"
        );
        self.notify_read.notify_waiting();
        self.position_reached.wait_until(&self.download_status, || {
            // Note: when `start == requested_end` at the known content length, that is an
            // exclusive end and is not "ready" yet. Waiting for the terminal state preserves the
            // original EOF synchronization and lets the download task finish its cleanup.
            self.downloaded
                .get(start)
                .is_some_and(|range| range.end >= requested_end)
        })
    }

    #[cfg(test)]
    pub(crate) fn wait_for_position(&self, requested_position: u64) -> io::Result<()> {
        self.request_position(requested_position);
        debug!(requested_position, "waiting for requested position");
        self.notify_read.notify_waiting();
        self.position_reached.wait_until(&self.download_status, || {
            requested_position == 0
                || self
                    .downloaded
                    .get(requested_position.saturating_sub(1))
                    .is_some_and(|range| range.end >= requested_position)
        })
    }

    pub(crate) fn seek(&self, seek_position: u64) -> io::Result<()> {
        self.request_seek(seek_position);
        debug!(
            requested_position = seek_position,
            "waiting for requested seek position"
        );
        self.position_reached.wait_until(&self.download_status, || {
            // A terminal completed state also covers seek-to-end, where there is no byte at
            // `seek_position` and therefore no downloaded range containing it.
            self.downloaded.get(seek_position).is_some()
        })
    }

    pub(crate) fn cancel(&self) {
        self.download_status.cancel();
        self.position_reached.notify_stream_done();
        self.notify_read.notify_cancelled();
    }

    pub(crate) fn notify_read(&self) {
        self.notify_read.notify_read();
    }

    pub(crate) fn content_length(&self) -> Option<u64> {
        self.content_length
    }

    #[cfg(test)]
    pub(crate) fn is_failed(&self) -> bool {
        self.download_status.is_failed()
    }

    #[cfg(test)]
    pub(crate) fn termination(&self) -> Option<DownloadTermination> {
        self.download_status.termination()
    }

    fn request_position(&self, position: u64) {
        self.requested_position.set(position);
    }

    fn request_seek(&self, position: u64) {
        // Order the shared request update and channel send together. Otherwise two concurrent
        // callers can update `requested_position` in one order but enqueue in the opposite order,
        // allowing the older position to become the final seek command.
        let (mutex, _) = self.position_reached.0.as_ref();
        {
            let _order_guard = mutex.lock();
            self.requested_position.set(position);

            // An unbounded channel cannot fill up, so concurrent seek requests are never silently
            // discarded. The download loop applies them in order; the newest position in that same
            // order is therefore always the final seek target.
            self.seek_tx.send(position).ok();
        }
    }
}

#[derive(Debug, Clone)]
pub(super) struct RequestedPosition(Arc<AtomicU64>);

const NO_REQUESTED_POSITION: u64 = u64::MAX;

impl Default for RequestedPosition {
    fn default() -> Self {
        Self(Arc::new(AtomicU64::new(NO_REQUESTED_POSITION)))
    }
}

// relaxed ordering as we are not using the atomic to
// lock something or to synchronize threads.
impl RequestedPosition {
    /// Clears the request only if it still contains `position`. This avoids an older download
    /// progression clearing a request that was replaced concurrently by a later reader.
    pub(super) fn clear_if(&self, position: u64) {
        let _ = self.0.compare_exchange(
            position,
            NO_REQUESTED_POSITION,
            Ordering::Relaxed,
            Ordering::Relaxed,
        );
    }

    pub(super) fn get(&self) -> Option<u64> {
        let val = self.0.load(Ordering::Relaxed);
        if val == NO_REQUESTED_POSITION {
            None
        } else {
            Some(val)
        }
    }

    fn set(&self, position: u64) {
        self.0.store(position, Ordering::Relaxed);
    }
}

#[derive(Default, Debug)]
struct Waiter {
    stream_done: bool,
}

// parking_lot's synchronization primitive's aren't unwind safe: https://github.com/Amanieu/parking_lot/issues/32
// Libraries that use FFI sometimes require these traits, which can cause compilation failures for
// anything that tries to hold a reference to this struct.
// Here we can use AssertUnwindSafe as long as we ensure we don't panic while holding a lock.
#[derive(Default, Clone, Debug)]
pub(super) struct PositionReached(Arc<(AssertUnwindSafe<Mutex<Waiter>>, Condvar)>);

impl PositionReached {
    pub(super) fn notify_position_reached(&self) {
        self.0.1.notify_all();
    }

    pub(super) fn notify_stream_done(&self) {
        let (mutex, cvar) = self.0.as_ref();
        mutex.lock().stream_done = true;
        cvar.notify_all();
    }

    /// Blocks until `ready` is true or the download reaches a terminal state.
    ///
    /// Unlike a single shared "position reached" flag, `ready` is owned by the caller and is
    /// rechecked after every notification and spurious wake-up. This lets any number of readers
    /// wait for different positions without consuming each other's wake-ups.
    pub(super) fn wait_until<F>(
        &self,
        download_status: &DownloadStatus,
        mut ready: F,
    ) -> io::Result<()>
    where
        F: FnMut() -> bool,
    {
        let wait_start = Instant::now();
        let (mutex, cvar) = self.0.as_ref();
        let mut waiter = mutex.lock();

        loop {
            if ready() {
                debug!(
                    elapsed = format!("{:?}", wait_start.elapsed()),
                    "requested data is available"
                );
                return Ok(());
            }

            if let Some(termination) = download_status.termination() {
                debug!(
                    ?termination,
                    elapsed = format!("{:?}", wait_start.elapsed()),
                    "download reached terminal state while waiting"
                );
                return termination.result();
            }

            if waiter.stream_done {
                // Some natural completion paths notify the condvar before `DownloadStatus` is
                // transitioned to `Completed`. Preserve the historical behavior of treating this
                // as EOF. Failure/cancellation paths always install their terminal status before
                // setting `stream_done`, so those are handled above.
                return Ok(());
            }

            cvar.wait(&mut waiter);
        }
    }
}

#[derive(Debug, Clone, Default)]
pub(super) struct Downloaded(Arc<AssertUnwindSafe<RwLock<RangeSet<u64>>>>);

impl Downloaded {
    pub(super) fn add(&self, range: Range<u64>) {
        // explicit check here prevents panic
        if range.end > range.start {
            self.0.write().insert(range);
        }
    }

    pub(super) fn get(&self, position: u64) -> Option<Range<u64>> {
        self.0.read().get(&position).cloned()
    }

    pub(super) fn next_gap(&self, range: Range<u64>) -> Option<Range<u64>> {
        self.0.read().gaps(&range).next()
    }
}

#[derive(Clone, Debug, Default)]
pub(super) struct NotifyRead {
    notify: Arc<Notify>,
    write_requested: Arc<AtomicBool>,
}

impl NotifyRead {
    pub(super) fn request(&self) {
        self.write_requested.store(true, Ordering::SeqCst);
    }

    fn notify_read(&self) {
        if self.write_requested.swap(false, Ordering::SeqCst) {
            self.notify.notify_one();
        }
    }

    fn notify_waiting(&self) {
        if self.write_requested.load(Ordering::SeqCst) {
            self.notify.notify_one();
        }
    }

    /// Wakes the download task if it is waiting for a reader. Cancellation is terminal, so the
    /// request flag is cleared and a notification is always issued to avoid a lost wake-up.
    pub(super) fn notify_cancelled(&self) {
        self.write_requested.store(false, Ordering::SeqCst);
        self.notify.notify_one();
    }

    pub(super) async fn wait_for_read(&self) {
        self.notify.notified().await;
    }
}

const STATUS_ACTIVE: u8 = 0;
const STATUS_FAILED: u8 = 1;
const STATUS_CANCELLED: u8 = 2;
const STATUS_COMPLETED: u8 = 3;

#[derive(Default, Clone, Debug)]
pub(super) struct DownloadStatus(Arc<AtomicU8>);

impl DownloadStatus {
    /// Unconditionally marks the download as failed.
    ///
    /// Prefer [`Self::fail_if_active`] on normal finish paths so an externally observed
    /// cancellation is not rewritten as a failure.
    #[cfg(test)]
    pub(super) fn set_failed(&self) {
        self.0.store(STATUS_FAILED, Ordering::SeqCst);
    }

    pub(super) fn fail_if_active(&self) -> bool {
        self.0
            .compare_exchange(
                STATUS_ACTIVE,
                STATUS_FAILED,
                Ordering::SeqCst,
                Ordering::SeqCst,
            )
            .is_ok()
    }

    pub(super) fn cancel(&self) -> bool {
        self.0
            .compare_exchange(
                STATUS_ACTIVE,
                STATUS_CANCELLED,
                Ordering::SeqCst,
                Ordering::SeqCst,
            )
            .is_ok()
    }

    pub(super) fn complete(&self) -> bool {
        self.0
            .compare_exchange(
                STATUS_ACTIVE,
                STATUS_COMPLETED,
                Ordering::SeqCst,
                Ordering::SeqCst,
            )
            .is_ok()
    }

    #[cfg(test)]
    fn is_failed(&self) -> bool {
        self.0.load(Ordering::SeqCst) == STATUS_FAILED
    }

    pub(super) fn termination(&self) -> Option<DownloadTermination> {
        match self.0.load(Ordering::SeqCst) {
            STATUS_FAILED => Some(DownloadTermination::Failed),
            STATUS_CANCELLED => Some(DownloadTermination::Cancelled),
            STATUS_COMPLETED => Some(DownloadTermination::Completed),
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Barrier;
    use std::thread;
    use std::time::Duration;

    use super::*;

    fn test_handle() -> (SourceHandle, mpsc::UnboundedReceiver<u64>) {
        let (seek_tx, seek_rx) = mpsc::unbounded_channel();
        let handle = SourceHandle {
            downloaded: Downloaded::default(),
            download_status: DownloadStatus::default(),
            requested_position: RequestedPosition::default(),
            position_reached: PositionReached::default(),
            content_length: None,
            seek_tx,
            notify_read: NotifyRead::default(),
        };
        (handle, seek_rx)
    }

    fn collect_results(
        rx: &std::sync::mpsc::Receiver<io::Result<()>>,
        count: usize,
    ) -> Vec<io::Result<()>> {
        (0..count)
            .map(|_| {
                rx.recv_timeout(Duration::from_secs(5))
                    .expect("waiting thread did not converge within 5 seconds")
            })
            .collect()
    }

    #[test]
    fn cancel_wakes_all_waiters_and_makes_later_waits_fail() {
        let (handle, _seek_rx) = test_handle();
        let barrier = Arc::new(Barrier::new(9));
        let (tx, rx) = std::sync::mpsc::channel();

        for i in 0..8 {
            let handle = handle.clone();
            let barrier = barrier.clone();
            let tx = tx.clone();
            thread::spawn(move || {
                barrier.wait();
                let result = if i % 2 == 0 {
                    handle.wait_for_position(100 + i)
                } else {
                    handle.wait_for_range(i * 10, 100 + i)
                };
                tx.send(result).expect("test receiver is alive");
            });
        }

        barrier.wait();
        thread::sleep(Duration::from_millis(20));
        handle.cancel();

        let results = collect_results(&rx, 8);
        assert!(results.iter().all(Result::is_err));
        assert_eq!(handle.termination(), Some(DownloadTermination::Cancelled));
        assert!(handle.wait_for_position(123).is_err());
        assert!(handle.seek(456).is_err());

        // Already-downloaded ranges remain readable after cancellation.
        handle.downloaded.add(0..64);
        handle.wait_for_range(0, 64).unwrap();
    }

    #[test]
    fn failure_wakes_blocked_waiters() {
        let (handle, _seek_rx) = test_handle();
        let (tx, rx) = std::sync::mpsc::channel();
        let mut threads = Vec::new();

        for _ in 0..4 {
            let handle = handle.clone();
            let tx = tx.clone();
            threads.push(thread::spawn(move || {
                tx.send(handle.wait_for_range(0, 10)).unwrap();
            }));
        }

        thread::sleep(Duration::from_millis(20));
        handle.download_status.set_failed();
        handle.position_reached.notify_stream_done();

        let results = collect_results(&rx, 4);
        assert!(results.iter().all(|result| {
            result
                .as_ref()
                .is_err_and(|e| e.kind() != io::ErrorKind::Interrupted)
        }));
        assert!(handle.is_failed());
        for thread in threads {
            thread.join().unwrap();
        }
    }

    #[test]
    fn completed_status_releases_waiters_as_eof_success() {
        let (handle, _seek_rx) = test_handle();
        handle.download_status.complete();
        handle.position_reached.notify_stream_done();

        handle.wait_for_position(u64::MAX - 1).unwrap();
        handle.seek(u64::MAX - 1).unwrap();
        assert_eq!(handle.termination(), Some(DownloadTermination::Completed));
    }

    #[test]
    fn progress_notifications_wake_independent_targets() {
        let (handle, _seek_rx) = test_handle();
        let (tx, rx) = std::sync::mpsc::channel();

        let targets = [(0_u64, 1_u64), (10, 20), (30, 40)];
        for (start, end) in targets {
            let handle = handle.clone();
            let tx = tx.clone();
            thread::spawn(move || {
                tx.send(handle.wait_for_range(start, end)).unwrap();
            });
        }

        thread::sleep(Duration::from_millis(20));
        handle.downloaded.add(0..40);
        handle.position_reached.notify_position_reached();

        let results = collect_results(&rx, targets.len());
        assert!(results.iter().all(Result::is_ok));
    }

    #[test]
    fn requested_position_clear_if_does_not_erase_newer_request() {
        let requested = RequestedPosition::default();
        requested.set(100);
        requested.set(200);

        requested.clear_if(100);
        assert_eq!(requested.get(), Some(200));

        requested.clear_if(200);
        assert_eq!(requested.get(), None);
    }

    #[test]
    fn seek_requests_are_never_dropped_by_a_full_channel() {
        let (handle, mut seek_rx) = test_handle();
        for position in 0..1000_u64 {
            handle.request_seek(position);
        }

        assert_eq!(seek_rx.len(), 1000);
        let mut latest = None;
        while let Ok(position) = seek_rx.try_recv() {
            latest = Some(position);
        }
        assert_eq!(latest, Some(999));
    }
}
