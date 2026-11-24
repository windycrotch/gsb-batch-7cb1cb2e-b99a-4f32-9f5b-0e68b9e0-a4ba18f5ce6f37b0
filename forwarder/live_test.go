package forwarder_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/forwarder"
	"github.com/alwitt/livemix/hls"
	"github.com/alwitt/livemix/mocks"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// testLiveForwarderRetryCfg fast retry config for tests
func testLiveForwarderRetryCfg() forwarder.SegmentForwardRetryConfig {
	return forwarder.SegmentForwardRetryConfig{
		MaxAttempts: 3,
		InitBackoff: time.Millisecond * 5,
		MaxBackoff:  time.Millisecond * 20,
	}
}

func newTestLiveSegment(sourceID string) common.VideoSegmentWithData {
	return common.VideoSegmentWithData{
		VideoSegment: common.VideoSegment{
			ID:       uuid.NewString(),
			SourceID: sourceID,
			Segment:  hls.Segment{Name: uuid.NewString()},
		},
		Content: []byte(uuid.NewString()),
	}
}

func TestHTTPLiveStreamForwarderNotStreaming(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	// Source is not streaming: the segment must NOT be reported as forwarded;
	// the caller must observe an explicit cancellation instead of a fake success.
	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: -1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	err = uut.ForwardSegment(utCtxt, newSegment, true)
	assert.Error(err)
	assert.True(
		errors.Is(err, forwarder.ErrLiveSegmentForwardCancelled),
		"expected cancellation error, got: %v", err,
	)

	// The sender must never have been invoked
	mockSender.AssertNotCalled(t, "ForwardSegment", mock.Anything, mock.Anything)

	// A cancelled segment may be resubmitted; it is not stuck in a failure state
	err = uut.ForwardSegment(utCtxt, newSegment, true)
	assert.True(errors.Is(err, forwarder.ErrLiveSegmentForwardCancelled))

	// Clean up
	assert.Nil(uut.Stop(utCtxt))
}

func TestHTTPLiveStreamForwarderSuccessAndDedup(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		segment, ok := args.Get(1).(common.VideoSegmentWithData)
		assert.True(ok)
		assert.Equal(testSource.ID, segment.SourceID)
		assert.Equal(newSegment.Name, segment.Name)
		assert.Equal(newSegment.Content, segment.Content)
	}).Return(nil).Once()

	// Case 1: source is streaming, segment is forwarded
	assert.Nil(uut.ForwardSegment(utCtxt, newSegment, true))

	// Case 2: resubmitting the same segment must not produce a duplicate success
	// record; the sender is not invoked again
	assert.Nil(uut.ForwardSegment(utCtxt, newSegment, true))
	mockSender.AssertNumberOfCalls(t, "ForwardSegment", 1)

	// Clean up
	assert.Nil(uut.Stop(utCtxt))
}

func TestHTTPLiveStreamForwarderRetryThenSuccess(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	// Control node rejects the first two attempts, then accepts
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(errors.New("control node unavailable")).Twice()
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()

	// The blocking caller only observes the final success
	assert.Nil(uut.ForwardSegment(utCtxt, newSegment, true))
	mockSender.AssertNumberOfCalls(t, "ForwardSegment", 3)

	// Clean up
	assert.Nil(uut.Stop(utCtxt))
}

func TestHTTPLiveStreamForwarderRetryExhausted(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	// Control node keeps rejecting
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(errors.New("control node unavailable"))

	// The blocking caller must observe the failure, not a silent loss
	err = uut.ForwardSegment(utCtxt, newSegment, true)
	assert.Error(err)
	mockSender.AssertNumberOfCalls(t, "ForwardSegment", 3)

	// The segment is now marked as failed: resubmitting it must not create a
	// duplicate (success) record, and must surface the recorded failure
	err = uut.ForwardSegment(utCtxt, newSegment, true)
	assert.Error(err)
	mockSender.AssertNumberOfCalls(t, "ForwardSegment", 3)

	// Clean up
	assert.Nil(uut.Stop(utCtxt))
}

func TestHTTPLiveStreamForwarderConcurrentDuplicate(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 4, nil,
	)
	assert.Nil(err)

	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	// The sender blocks until the test releases it
	senderEntered := make(chan struct{}, 1)
	senderGate := make(chan struct{})
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		senderEntered <- struct{}{}
		<-senderGate
	}).Return(nil).Once()

	// Submit the same segment concurrently from multiple callers
	const callers = 5
	var wg sync.WaitGroup
	results := make([]error, callers)
	for itr := 0; itr < callers; itr++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = uut.ForwardSegment(utCtxt, newSegment, true)
		}(itr)
	}

	// Wait for the forward to actually start, then release it
	select {
	case <-senderEntered:
	case <-time.After(time.Second * 5):
		assert.Fail("timed out waiting for the sender to be invoked")
	}
	close(senderGate)
	wg.Wait()

	// All callers observe the same single successful delivery
	for itr := 0; itr < callers; itr++ {
		assert.Nil(results[itr])
	}
	mockSender.AssertNumberOfCalls(t, "ForwardSegment", 1)

	// Clean up
	assert.Nil(uut.Stop(utCtxt))
}

func TestHTTPLiveStreamForwarderStopCancelsInFlight(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()
	mockSender := mocks.NewSegmentSender(t)

	uut, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockSender, testLiveForwarderRetryCfg(), 2, nil,
	)
	assert.Nil(err)

	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	newSegment := newTestLiveSegment(testSource.ID)

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	// The sender blocks until its context is cancelled (i.e. the forwarder stops)
	senderEntered := make(chan struct{}, 1)
	mockSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Run(func(args mock.Arguments) {
		senderEntered <- struct{}{}
		ctxt, ok := args.Get(0).(context.Context)
		assert.True(ok)
		<-ctxt.Done()
	}).Return(context.Canceled)

	// One blocking caller waits for the in-flight segment
	callResult := make(chan error, 1)
	go func() {
		callResult <- uut.ForwardSegment(utCtxt, newSegment, true)
	}()

	// Wait for the segment to be in-flight
	select {
	case <-senderEntered:
	case <-time.After(time.Second * 5):
		assert.Fail("timed out waiting for the sender to be invoked")
	}

	// Stop the forwarder while the segment is in-flight
	assert.Nil(uut.Stop(utCtxt))

	// The blocking caller is released with an explicit cancellation, never with a
	// fake success
	select {
	case err := <-callResult:
		assert.Error(err)
		assert.True(
			errors.Is(err, forwarder.ErrLiveSegmentForwardCancelled),
			"expected cancellation error, got: %v", err,
		)
	case <-time.After(time.Second * 5):
		assert.Fail("blocking caller was not released by Stop")
	}

	// After stop, new submissions are rejected rather than silently accepted
	assert.Error(uut.ForwardSegment(utCtxt, newTestLiveSegment(testSource.ID), true))
}
