package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testChannel types.ChannelID = 4242

// startTestStream starts a stream through the real StartStream entry point and
// asserts it becomes live. The blocking ingest is replaced by a context-wait
// fake in the harness.
func startTestStream(t *testing.T, h *testHarness) *Stream {
	t.Helper()

	stream, err := h.ctrl.StartStream(testChannel)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.True(t, h.ctrl.hasStream(testChannel), "stream should be in the active-stream table")
	require.Equal(t, 1, h.svc.startCount())
	require.Equal(t, 1, h.or.startCount())

	return stream
}

// TestStartThenStop_HappyPath covers: normal publish followed by a publisher
// disconnect. The external end notification must fire exactly once and the
// channel must return to a clean, re-publishable state.
func TestStartThenStop_HappyPath(t *testing.T) {
	h := newTestHarness(t)

	stream := startTestStream(t, h)

	// Simulate the publisher disconnecting.
	require.NoError(t, h.ctrl.StopStream(testChannel))

	assert.Equal(t, 1, h.svc.endCount(), "service EndStream must be called exactly once")
	assert.Equal(t, 1, h.or.stopCount(), "orchestrator StopStream must be called exactly once")
	assert.False(t, h.ctrl.hasStream(testChannel), "stream must be removed from the local table")
	assert.True(t, stream.Stopped(), "stream context must be cancelled")
	assert.Equal(t, 0, h.ctrl.activeStreamCount())

	// The channel must be immediately re-publishable (no "stream already exists").
	stream2, err := h.ctrl.StartStream(testChannel)
	require.NoError(t, err)
	require.NotNil(t, stream2)
	assert.True(t, h.ctrl.hasStream(testChannel))

	require.NoError(t, h.ctrl.StopStream(testChannel))
	assert.Equal(t, 2, h.svc.endCount(), "each live session gets exactly one end notification")
	assert.False(t, h.ctrl.hasStream(testChannel))
}

// TestStopStream_DuplicateIsIdempotent covers two end signals arriving close
// together (publisher disconnect + player delete). The second signal must not
// surface a "stream not found" error and must not trigger a second external
// end call.
func TestStopStream_DuplicateIsIdempotent(t *testing.T) {
	h := newTestHarness(t)
	startTestStream(t, h)

	// Sequential duplicate: the second call happens after teardown completed.
	require.NoError(t, h.ctrl.StopStream(testChannel))
	err := h.ctrl.StopStream(testChannel)
	require.NoError(t, err, "duplicate stop after removal must not error")

	assert.Equal(t, 1, h.svc.endCount())
	assert.Equal(t, 1, h.or.stopCount())
	assert.False(t, h.ctrl.hasStream(testChannel))

	// Stopping a channel that never existed must also be a harmless no-op.
	require.NoError(t, h.ctrl.StopStream(types.ChannelID(9999)))
	assert.Equal(t, 1, h.svc.endCount())
}

// TestStopStream_ConcurrentSignals fires many stop signals concurrently while
// the first teardown is deliberately held open inside the external service
// call. Only one external end call is permitted, and every caller must return
// without an error and observe the final clean state.
func TestStopStream_ConcurrentSignals(t *testing.T) {
	h := newTestHarness(t)
	startTestStream(t, h)

	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	h.svc.mu.Lock()
	h.svc.endErr = nil
	h.svc.mu.Unlock()

	// Wrap EndStream via a blocking fake: hold all stop callers inside the
	// single teardown until we release it.
	h.ctrl.service = &blockingService{
		fake:    h.svc,
		entered: entered,
		release: release,
		once:    &once,
	}

	const callers = 16
	var wg sync.WaitGroup
	errs := make([]error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = h.ctrl.StopStream(testChannel)
		}(i)
	}

	// Wait until the owning caller is parked inside EndStream, then give the
	// remaining callers time to arrive and block on the teardown barrier.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never entered the external end call")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "concurrent stop caller %d must not error", i)
	}
	assert.Equal(t, 1, h.svc.endCount(), "external EndStream invoked exactly once under concurrency")
	assert.Equal(t, 1, h.or.stopCount(), "orchestrator StopStream invoked exactly once under concurrency")
	assert.False(t, h.ctrl.hasStream(testChannel))
}

// TestStopStream_ExternalErrorStillCleansUp covers the external service failing
// during end. The error must be observable (returned + logged) but the stream
// must still be removed locally so the next publish is not blocked.
func TestStopStream_ExternalErrorStillCleansUp(t *testing.T) {
	h := newTestHarness(t)
	stream := startTestStream(t, h)

	endErr := errors.New("external service exploded")
	h.svc.endErr = endErr

	err := h.ctrl.StopStream(testChannel)
	require.Error(t, err, "the owning caller must observe the external failure")
	assert.ErrorIs(t, err, endErr)

	assert.Equal(t, 1, h.svc.endCount(), "the failing EndStream is still attempted exactly once")
	assert.Equal(t, 1, h.or.stopCount(), "orchestrator is still notified")
	assert.False(t, h.ctrl.hasStream(testChannel), "local table must be cleaned despite external error")
	assert.True(t, stream.Stopped())

	// A duplicate stop after the failed teardown must not re-invoke the external
	// service and must not return an error to the second caller.
	require.NoError(t, h.ctrl.StopStream(testChannel))
	assert.Equal(t, 1, h.svc.endCount(), "failed end must not be retried by a duplicate caller")

	// The channel must accept a new publish immediately.
	stream2, err := h.ctrl.StartStream(testChannel)
	require.NoError(t, err)
	require.NotNil(t, stream2)
	assert.True(t, h.ctrl.hasStream(testChannel))

	// Reset the external failure and confirm the new session tears down cleanly.
	h.svc.endErr = nil
	require.NoError(t, h.ctrl.StopStream(testChannel))
	assert.Equal(t, 2, h.svc.endCount())
	assert.False(t, h.ctrl.hasStream(testChannel))
}

// TestHeartbeatFailure_TriggersFullTeardown covers the heartbeat loop deciding
// to stop the stream after consecutive failures. It must take the same path as
// a normal disconnect: one external end notification and a clean local table.
func TestHeartbeatFailure_TriggersFullTeardown(t *testing.T) {
	h := newTestHarness(t)

	// Make heartbeats tick fast and always fail so the threshold is reached
	// deterministically and quickly.
	h.ctrl.heartbeatInterval = 5 * time.Millisecond
	h.or.heartbeatErr = errors.New("orchestrator heartbeat unreachable")
	// Thumbnail/metadata also fail to mirror a fully unhealthy stream.
	h.svc.mu.Lock()
	h.svc.forceMetadataErr = true
	h.svc.mu.Unlock()

	stream := startTestStream(t, h)

	deadline := time.After(3 * time.Second)
	for h.ctrl.hasStream(testChannel) {
		select {
		case <-deadline:
			t.Fatal("stream was not torn down after heartbeat failures")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	assert.True(t, stream.Stopped(), "heartbeat-driven teardown must cancel the stream")
	assert.Equal(t, 1, h.svc.endCount(), "heartbeat stop must still notify the external service once")
	assert.Equal(t, 1, h.or.stopCount(), "heartbeat stop must still notify the orchestrator once")
	assert.False(t, h.ctrl.hasStream(testChannel))
	assert.Equal(t, 0, h.ctrl.activeStreamCount())

	// Re-publish must work.
	stream2, err := h.ctrl.StartStream(testChannel)
	require.NoError(t, err)
	require.NotNil(t, stream2)
}

// TestShutdown_TearsDownAllStreams covers server shutdown: every active stream
// goes through the same idempotent teardown, each external service receives one
// end notification per channel, and the local table ends empty.
func TestShutdown_TearsDownAllStreams(t *testing.T) {
	h := newTestHarness(t)

	const n = 5
	channels := make([]types.ChannelID, n)
	for i := range channels {
		channels[i] = types.ChannelID(100 + i)
		_, err := h.ctrl.StartStream(channels[i])
		require.NoError(t, err)
	}
	require.Equal(t, n, h.ctrl.activeStreamCount())

	h.ctrl.Shutdown()

	assert.Equal(t, 0, h.ctrl.activeStreamCount(), "no active streams may remain after shutdown")
	assert.Equal(t, n, h.svc.endCount(), "one external end notification per channel")
	assert.Equal(t, n, h.or.stopCount(), "one orchestrator stop per channel")
	for _, c := range channels {
		assert.False(t, h.ctrl.hasStream(c))
	}

	// A second shutdown (nothing live) must be a safe no-op and must not invoke
	// any further external calls.
	h.ctrl.Shutdown()
	assert.Equal(t, n, h.svc.endCount())
	assert.Equal(t, n, h.or.stopCount())
}

// TestIngestReturn_TriggersTeardown models the actual publisher-disconnect
// path: the blocking ingest returns on its own (as Stream.Ingest does when the
// peer connection ends). StartStream's goroutine must then run the same
// idempotent teardown without any explicit StopStream call.
func TestIngestReturn_TriggersTeardown(t *testing.T) {
	h := newTestHarness(t)

	ingestReturned := make(chan struct{})
	// An ingest that returns immediately, simulating the publisher dropping.
	h.ctrl.runIngest = func(_ *Stream, _ context.Context) error {
		close(ingestReturned)

		return nil
	}

	stream, err := h.ctrl.StartStream(testChannel)
	require.NoError(t, err)

	select {
	case <-ingestReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("ingest never returned")
	}

	deadline := time.After(2 * time.Second)
	for h.ctrl.hasStream(testChannel) {
		select {
		case <-deadline:
			t.Fatal("ingest return did not converge into a full teardown")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	assert.True(t, stream.Stopped())
	assert.Equal(t, 1, h.svc.endCount(), "ingest-driven teardown still notifies the service exactly once")
	assert.Equal(t, 1, h.or.stopCount())
	assert.False(t, h.ctrl.hasStream(testChannel))
	assert.Equal(t, 0, h.ctrl.activeStreamCount())
}

// TestStreamStop_IsIdempotent verifies the low-level Stream.Stop primitive can
// be hammered concurrently without panicking or blocking and only flips state
// once.
func TestStreamStop_IsIdempotent(t *testing.T) {
	h := newTestHarness(t)
	stream := startTestStream(t, h)

	require.False(t, stream.Stopped())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NotPanics(t, func() { stream.Stop() })
		}()
	}
	wg.Wait()

	assert.True(t, stream.Stopped())

	// Clean up so the test leaves no live stream behind.
	require.NoError(t, h.ctrl.StopStream(testChannel))
}

// blockingService wraps fakeService and blocks the first EndStream call until
// release is closed, letting the concurrency test hold the teardown open.
type blockingService struct {
	fake    *fakeService
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (b *blockingService) SetLogger(l logrus.FieldLogger) { b.fake.SetLogger(l) }
func (b *blockingService) Name() string                   { return b.fake.Name() }
func (b *blockingService) Connect() error                 { return b.fake.Connect() }
func (b *blockingService) GetHmacKey(c types.ChannelID) ([]byte, error) {
	return b.fake.GetHmacKey(c)
}
func (b *blockingService) StartStream(c types.ChannelID) (types.StreamID, error) {
	return b.fake.StartStream(c)
}
func (b *blockingService) EndStream(id types.StreamID) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release

	return b.fake.EndStream(id)
}
func (b *blockingService) UpdateStreamMetadata(id types.StreamID, m types.StreamMetadata) error {
	return b.fake.UpdateStreamMetadata(id, m)
}
func (b *blockingService) SendJpegPreviewImage(id types.StreamID, img []byte) error {
	return b.fake.SendJpegPreviewImage(id, img)
}
