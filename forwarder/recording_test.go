package forwarder_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/common/ipc"
	"github.com/alwitt/livemix/forwarder"
	"github.com/alwitt/livemix/hls"
	"github.com/alwitt/livemix/mocks"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// testRecordingForwarderRetryCfg fast retry config for tests
func testRecordingForwarderRetryCfg() forwarder.SegmentForwardRetryConfig {
	return forwarder.SegmentForwardRetryConfig{
		MaxAttempts: 2,
		InitBackoff: time.Millisecond * 5,
		MaxBackoff:  time.Millisecond * 20,
	}
}

func newTestRecordingSegments(sourceID string, count int) []common.VideoSegmentWithData {
	segments := []common.VideoSegmentWithData{}
	for itr := 0; itr < count; itr++ {
		segments = append(segments, common.VideoSegmentWithData{
			VideoSegment: common.VideoSegment{
				ID:       uuid.NewString(),
				SourceID: sourceID,
				Segment: hls.Segment{
					Name: fmt.Sprintf("%s.ts", uuid.NewString()),
				},
			},
			Content: []byte(uuid.NewString()),
		})
	}
	return segments
}

// broadcastRecorder collects recording segment reports emitted by the broadcaster mock
type broadcastRecorder struct {
	lock     sync.Mutex
	reports  []ipc.RecordingSegmentReport
	notifier chan struct{}
}

func newBroadcastRecorder() *broadcastRecorder {
	return &broadcastRecorder{notifier: make(chan struct{}, 64)}
}

func (r *broadcastRecorder) record(msg *ipc.RecordingSegmentReport) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.reports = append(r.reports, *msg)
	select {
	case r.notifier <- struct{}{}:
	default:
	}
}

// countPerSession count how many reports exist per (session, segment) pair
func (r *broadcastRecorder) countPerSession() map[string]map[string]int {
	r.lock.Lock()
	defer r.lock.Unlock()
	result := map[string]map[string]int{}
	for _, report := range r.reports {
		for _, session := range report.RecordingIDs {
			if _, ok := result[session]; !ok {
				result[session] = map[string]int{}
			}
			for _, segment := range report.Segments {
				result[session][segment.ID]++
			}
		}
	}
	return result
}

func (r *broadcastRecorder) total() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return len(r.reports)
}

// waitForReports wait until at least `count` reports were recorded
func (r *broadcastRecorder) waitForReports(t *testing.T, count int) {
	deadline := time.After(time.Second * 10)
	for {
		if r.total() >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d reports; got %d", count, r.total())
		case <-r.notifier:
		}
	}
}

func TestS3RecordingSegmentForwarder(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	// ------------------------------------------------------------------------------------
	// Upload segments; each recording session receives its own report per segment

	{
		testSourceID := uuid.NewString()
		testRecordings := []string{
			uuid.NewString(), uuid.NewString(), uuid.NewString(),
		}
		testSegments := newTestRecordingSegments(testSourceID, 2)
		testSegmentByID := map[string]common.VideoSegmentWithData{}
		for _, segment := range testSegments {
			testSegmentByID[segment.ID] = segment
		}
		recorder := newBroadcastRecorder()

		// Prepare mocks
		receivedSegment := map[string]bool{}
		mockS3.On(
			"ForwardSegment",
			mock.AnythingOfType("*context.cancelCtx"),
			mock.AnythingOfType("common.VideoSegmentWithData"),
		).Run(func(args mock.Arguments) {
			segment, ok := args.Get(1).(common.VideoSegmentWithData)
			assert.True(ok)
			assert.Contains(testSegmentByID, segment.ID)
			expected := testSegmentByID[segment.ID]
			assert.Equal(expected.SourceID, segment.SourceID)
			assert.Equal(expected.Name, segment.Name)
			assert.EqualValues(expected.Content, segment.Content)
			receivedSegment[segment.ID] = true
		}).Return(nil).Times(len(testSegments))
		mockBroadcaster.On(
			"Broadcast",
			mock.AnythingOfType("*context.cancelCtx"),
			mock.Anything,
		).Run(func(args mock.Arguments) {
			msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
			assert.True(ok)
			recorder.record(msg)
		}).Return(nil)

		// Call
		assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

		// Each session receives its own report for each segment
		recorder.waitForReports(t, len(testSegments)*len(testRecordings))
		counts := recorder.countPerSession()
		assert.Len(counts, len(testRecordings))
		for _, session := range testRecordings {
			assert.Len(counts[session], len(testSegments))
			for _, segment := range testSegments {
				assert.Equal(1, counts[session][segment.ID])
			}
		}
		assert.Len(receivedSegment, len(testSegments))
	}

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderResubmitDedup(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	// Resubmitting the same segments must not duplicate any report
	testSourceID := uuid.NewString()
	testRecordings := []string{uuid.NewString(), uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 1)
	recorder := newBroadcastRecorder()

	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	// First submission
	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))
	recorder.waitForReports(t, len(testRecordings))

	// Resubmit the same segment for the same sessions
	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

	// Give any erroneous duplicate a chance to appear
	time.Sleep(time.Millisecond * 200)

	counts := recorder.countPerSession()
	for _, session := range testRecordings {
		assert.Equal(1, counts[session][testSegments[0].ID])
	}
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderAlreadyUploaded(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	testRecordings := []string{uuid.NewString(), uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 2)
	// The first segment was already uploaded according to persistence
	uploaded := 1
	testSegments[0].Uploaded = &uploaded
	recorder := newBroadcastRecorder()

	// Only the second segment is uploaded
	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		segment, ok := args.Get(1).(common.VideoSegmentWithData)
		assert.True(ok)
		assert.Equal(testSegments[1].ID, segment.ID)
	}).Return(nil).Once()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

	// Both segments are reported to every session, but only one upload happened
	recorder.waitForReports(t, len(testSegments)*len(testRecordings))
	counts := recorder.countPerSession()
	for _, session := range testRecordings {
		for _, segment := range testSegments {
			assert.Equal(1, counts[session][segment.ID])
		}
	}
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderUploadFailureNoBroadcast(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	testRecordings := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 1)
	recorder := newBroadcastRecorder()

	// The object store keeps failing during the first submission
	uploadAttempts := make(chan struct{}, 16)
	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		uploadAttempts <- struct{}{}
	}).Return(errors.New("object store unavailable")).Times(
		testRecordingForwarderRetryCfg().MaxAttempts,
	)
	// No broadcast may happen while the write fails
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

	// Wait for the retries to be exhausted
	for itr := 0; itr < testRecordingForwarderRetryCfg().MaxAttempts; itr++ {
		select {
		case <-uploadAttempts:
		case <-time.After(time.Second * 5):
			assert.Fail("timed out waiting for upload attempts")
		}
	}
	// Give any erroneous broadcast a chance to appear
	time.Sleep(time.Millisecond * 200)
	assert.Equal(0, recorder.total(), "no broadcast may happen when the write fails")

	// The object store recovers; resubmitting the same segment completes the
	// store-and-report pipeline exactly once per session
	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()

	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))
	recorder.waitForReports(t, len(testRecordings))

	counts := recorder.countPerSession()
	for _, session := range testRecordings {
		assert.Equal(1, counts[session][testSegments[0].ID])
	}

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderBroadcastFailure(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	sessionOK := uuid.NewString()
	sessionFailing := uuid.NewString()
	testRecordings := []string{sessionOK, sessionFailing}
	testSegments := newTestRecordingSegments(testSourceID, 1)
	recorder := newBroadcastRecorder()

	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()

	// The broadcast targeting the failing session errors out on the first attempt
	failedOnce := make(chan struct{}, 1)
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.MatchedBy(func(msg interface{}) bool {
			report, ok := msg.(*ipc.RecordingSegmentReport)
			return ok && len(report.RecordingIDs) == 1 && report.RecordingIDs[0] == sessionFailing
		}),
	).Run(func(args mock.Arguments) {
		failedOnce <- struct{}{}
	}).Return(errors.New("broadcast bus unavailable")).Once()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

	// Wait for the failing session's broadcast to fail
	select {
	case <-failedOnce:
	case <-time.After(time.Second * 5):
		assert.Fail("timed out waiting for the failing broadcast")
	}
	// The healthy session was reported exactly once
	recorder.waitForReports(t, 1)

	// Resubmit: only the missing session report is emitted; no new upload happens
	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))
	recorder.waitForReports(t, 2)

	time.Sleep(time.Millisecond * 200)
	counts := recorder.countPerSession()
	assert.Equal(1, counts[sessionOK][testSegments[0].ID])
	assert.Equal(1, counts[sessionFailing][testSegments[0].ID])
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderConcurrentDuplicate(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 4, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	testRecordings := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 1)
	recorder := newBroadcastRecorder()

	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	// Submit the same segment for the same sessions concurrently
	const callers = 5
	var wg sync.WaitGroup
	for itr := 0; itr < callers; itr++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))
		}()
	}
	wg.Wait()

	// Exactly one upload, and exactly one report per session
	recorder.waitForReports(t, len(testRecordings))
	time.Sleep(time.Millisecond * 200)
	counts := recorder.countPerSession()
	assert.Len(counts, len(testRecordings))
	for _, session := range testRecordings {
		assert.Equal(1, counts[session][testSegments[0].ID])
	}
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderSharedSegmentAcrossSessions(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	sessionsA := []string{uuid.NewString(), uuid.NewString()}
	sessionsB := []string{uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 1)
	recorder := newBroadcastRecorder()

	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	// The same segment is shared by two recording session groups (e.g. a new
	// recording session started after the segment was already recorded)
	assert.Nil(uut.ForwardSegment(utCtxt, sessionsA, testSegments))
	recorder.waitForReports(t, len(sessionsA))
	assert.Nil(uut.ForwardSegment(utCtxt, sessionsB, testSegments))
	recorder.waitForReports(t, len(sessionsA)+len(sessionsB))

	time.Sleep(time.Millisecond * 200)
	counts := recorder.countPerSession()
	// Every session received its own report, exactly once
	for _, session := range append(sessionsA, sessionsB...) {
		assert.Equal(1, counts[session][testSegments[0].ID])
	}
	// The segment content was uploaded only once
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(uut.Stop(utCtxt))
}

func TestS3RecordingSegmentForwarderStopCancelsInFlight(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	storageCfg := common.RecordingStorageConfig{
		StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
	}

	uut, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt, storageCfg, mockS3, mockBroadcaster, testRecordingForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSourceID := uuid.NewString()
	testRecordings := []string{uuid.NewString()}
	testSegments := newTestRecordingSegments(testSourceID, 1)

	// The upload blocks until its context is cancelled (i.e. the forwarder stops)
	uploadStarted := make(chan struct{}, 1)
	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		uploadStarted <- struct{}{}
		ctxt, ok := args.Get(0).(context.Context)
		assert.True(ok)
		<-ctxt.Done()
	}).Return(context.Canceled)
	// No Broadcast expectation is installed: the mock fails this test if any
	// "segment stored" event is emitted for the interrupted write

	assert.Nil(uut.ForwardSegment(utCtxt, testRecordings, testSegments))

	// Wait for the upload to be in-flight, then stop the forwarder
	select {
	case <-uploadStarted:
	case <-time.After(time.Second * 5):
		assert.Fail("timed out waiting for the upload to start")
	}
	assert.Nil(uut.Stop(utCtxt))
}
