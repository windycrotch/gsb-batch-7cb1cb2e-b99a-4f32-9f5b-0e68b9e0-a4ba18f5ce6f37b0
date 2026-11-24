use std::io::Read;
use std::pin::Pin;
use std::task::{Context, Poll};
use std::time::Duration;

use stream_download::async_read::AsyncReadStreamParams;
use stream_download::storage::memory::MemoryStorageProvider;
use stream_download::{Settings, StreamDownload};
use tokio::io::{AsyncRead, ReadBuf};

struct NeverReady;

impl AsyncRead for NeverReady {
    fn poll_read(
        self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
        _buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Poll::Pending
    }
}

#[test]
fn cancellation_is_observable_to_blocked_and_future_reads() {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();

    runtime.block_on(async move {
        let reader = StreamDownload::new_async_read(
            AsyncReadStreamParams::new(NeverReady),
            MemoryStorageProvider,
            Settings::default()
                .prefetch_bytes(0)
                .retry_timeout(Duration::from_millis(20)),
        )
        .await
        .unwrap();

        let external_cancellation = reader.cancellation_token();
        let (first_result_tx, first_result_rx) = std::sync::mpsc::channel();

        let reader_thread = std::thread::spawn(move || {
            let mut reader = reader;
            let mut buf = [0_u8; 1];

            let first_result = reader.read(&mut buf);
            first_result_tx
                .send(first_result.is_err())
                .expect("test receiver remains alive");

            // Calling the public cancellation entry point again must keep the terminal state.
            reader.cancel_download();

            let second_result = reader.read(&mut buf);
            assert!(second_result.is_err(), "later reads must not wait forever");
        });

        // Give the blocking read time to enter the condition variable wait.
        tokio::time::sleep(Duration::from_millis(100)).await;
        external_cancellation.cancel();

        let first_read_failed = first_result_rx
            .recv_timeout(Duration::from_secs(5))
            .expect("blocking read was not cancelled within 5 seconds");
        assert!(first_read_failed, "the blocked read must return an error");

        reader_thread
            .join()
            .expect("reader thread should converge after cancellation");
    });
}
