package control

import (
	"context"
	"errors"
	"image"
	"sync"
	"testing"

	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/sirupsen/logrus"
)

// fakeService is a controllable in-memory implementation of service.Service.
// It records every external call and can be configured to fail EndStream on
// demand, so the teardown error handling can be exercised deterministically.
type fakeService struct {
	mu sync.Mutex

	startCalls   int
	endCalls     int
	endStreamIDs []types.StreamID
	metadataN    int
	thumbnailN   int
	hmacN        int

	// endErr, when non-nil, is returned by EndStream.
	endErr error
	// startErr, when non-nil, is returned by StartStream.
	startErr error
	// forceMetadataErr makes UpdateStreamMetadata fail (used by heartbeat tests).
	forceMetadataErr bool
}

func (f *fakeService) SetLogger(_ logrus.FieldLogger) {}

func (f *fakeService) Name() string { return "fake-service" }

func (f *fakeService) Connect() error { return nil }

func (f *fakeService) GetHmacKey(_ types.ChannelID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hmacN++

	return []byte("test-hmac-key"), nil
}

func (f *fakeService) StartStream(channelID types.ChannelID) (types.StreamID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++

	if f.startErr != nil {
		return 0, f.startErr
	}

	return types.StreamID(channelID + 1), nil
}

func (f *fakeService) EndStream(streamID types.StreamID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endCalls++
	f.endStreamIDs = append(f.endStreamIDs, streamID)

	return f.endErr
}

func (f *fakeService) UpdateStreamMetadata(_ types.StreamID, _ types.StreamMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metadataN++

	if f.forceMetadataErr {
		return errors.New("fake metadata update failure")
	}

	return nil
}

func (f *fakeService) SendJpegPreviewImage(_ types.StreamID, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thumbnailN++

	return nil
}

func (f *fakeService) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startCalls
}

func (f *fakeService) endCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.endCalls
}

// fakeOrchestrator is a controllable in-memory Orchestrator. It records
// lifecycle and heartbeat calls and can fail either on demand.
type fakeOrchestrator struct {
	mu sync.Mutex

	startCalls    int
	stopCalls     int
	stopChannelID []types.ChannelID
	heartbeatN    int

	stopErr      error
	startErr     error
	heartbeatErr error
}

func (f *fakeOrchestrator) Name() string { return "fake-orchestrator" }

func (f *fakeOrchestrator) Connect() error { return nil }

func (f *fakeOrchestrator) Close() error { return nil }

func (f *fakeOrchestrator) SetLogger(_ logrus.FieldLogger) {}

func (f *fakeOrchestrator) StartStream(channelID types.ChannelID, _ types.StreamID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++

	return f.startErr
}

func (f *fakeOrchestrator) StopStream(channelID types.ChannelID, _ types.StreamID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.stopChannelID = append(f.stopChannelID, channelID)

	return f.stopErr
}

func (f *fakeOrchestrator) Heartbeat(_ types.ChannelID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeatN++

	return f.heartbeatErr
}

func (f *fakeOrchestrator) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startCalls
}

func (f *fakeOrchestrator) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.stopCalls
}

// fakeImageDecoder satisfies imageDecoder without touching ffmpeg.
type fakeImageDecoder struct {
	closed bool
}

func (d *fakeImageDecoder) Decode(_ []byte) (image.Image, error) {
	return image.NewRGBA(image.Rect(0, 0, 2, 2)), nil
}

func (d *fakeImageDecoder) Close() { d.closed = true }

// testHarness bundles a Control wired to fake dependencies plus the handles a
// test needs to make assertions.
type testHarness struct {
	ctrl    *Control
	svc     *fakeService
	or      *fakeOrchestrator
	ctx     context.Context
	cancel  context.CancelFunc
	decoder *fakeImageDecoder
}

func newTestHarness(tb testing.TB) *testHarness {
	tb.Helper()

	logger := logrus.New()
	logger.SetLevel(logrus.PanicLevel) // keep test output quiet; teardown errors still returned

	ctx, cancel := context.WithCancel(context.Background())
	svc := &fakeService{}
	or := &fakeOrchestrator{}
	decoder := &fakeImageDecoder{}

	ctrl := newControlWithDeps(ctx, svc, or, logger)
	// Default ingest blocks until the Control's own context is cancelled, i.e.
	// for the whole test. This keeps the stream live until the test explicitly
	// calls StopStream, so teardown timing is fully controlled by the test and
	// there is no background goroutine racing the teardown state machine. Tests
	// that need ingest-driven teardown override runIngest individually.
	ctrl.runIngest = func(_ *Stream, _ context.Context) error {
		<-ctx.Done()

		return nil
	}
	// Avoid any cgo decoder in thumbnail handling.
	prevDecoderFactory := newImageDecoder
	newImageDecoder = func() (imageDecoder, error) { return decoder, nil }

	tb.Cleanup(func() {
		newImageDecoder = prevDecoderFactory
		cancel()
	})

	return &testHarness{
		ctrl:    ctrl,
		svc:     svc,
		or:      or,
		ctx:     ctx,
		cancel:  cancel,
		decoder: decoder,
	}
}
