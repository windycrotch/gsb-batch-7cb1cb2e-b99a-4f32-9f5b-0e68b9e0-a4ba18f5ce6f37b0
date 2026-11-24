package forwarder_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/common/ipc"
	"github.com/alwitt/livemix/forwarder"
	"github.com/alwitt/livemix/mocks"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

/*
TestForwardingPipelinesAreIndependent verify that the live forwarding chain and the
recording chain, though processing the same segment data, do not affect each other:
a failing live forward must not disturb recording ingestion, and a failing recording
write must not make the live forward misreport its state.
*/
func TestForwardingPipelinesAreIndependent(t *testing.T) {
	assert := assert.New(t)
	log.SetLevel(log.DebugLevel)

	utCtxt := context.Background()

	// Shared infrastructure mocks
	mockSQL := mocks.NewConnectionManager(t)
	mockDB := mocks.NewPersistenceManager(t)
	mockSQL.On("NewPersistanceManager").Return(mockDB)
	mockDB.On("Close").Return()

	// Live chain: the control node keeps rejecting
	mockLiveSender := mocks.NewSegmentSender(t)
	// Recording chain: storage and broadcast work fine
	mockS3 := mocks.NewSegmentSender(t)
	mockBroadcaster := mocks.NewBroadcaster(t)

	retryCfg := forwarder.SegmentForwardRetryConfig{
		MaxAttempts: 2,
		InitBackoff: time.Millisecond * 5,
		MaxBackoff:  time.Millisecond * 10,
	}

	liveForwarder, err := forwarder.NewHTTPLiveStreamSegmentForwarder(
		utCtxt, mockSQL, mockLiveSender, retryCfg, 2, nil,
	)
	assert.Nil(err)
	recordingForwarder, err := forwarder.NewS3RecordingSegmentForwarder(
		utCtxt,
		common.RecordingStorageConfig{
			StorageBucket: uuid.NewString(), StorageObjectPrefix: uuid.NewString(),
		},
		mockS3,
		mockBroadcaster,
		retryCfg,
		2,
		nil,
	)
	assert.Nil(err)

	// The segment data shared by both chains
	testSource := common.VideoSource{ID: uuid.NewString(), Streaming: 1}
	sharedSegment := newTestLiveSegment(testSource.ID)
	testRecordings := []string{uuid.NewString(), uuid.NewString()}

	mockDB.On(
		"GetVideoSource",
		mock.AnythingOfType("*context.cancelCtx"),
		testSource.ID,
	).Return(testSource, nil)

	// Live chain fails on every attempt
	mockLiveSender.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(errors.New("control node unavailable"))

	// Recording chain works
	mockS3.On(
		"ForwardSegment",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.AnythingOfType("common.VideoSegmentWithData"),
	).Return(nil).Once()
	recorder := newBroadcastRecorder()
	mockBroadcaster.On(
		"Broadcast",
		mock.AnythingOfType("*context.cancelCtx"),
		mock.Anything,
	).Run(func(args mock.Arguments) {
		msg, ok := args.Get(1).(*ipc.RecordingSegmentReport)
		assert.True(ok)
		recorder.record(msg)
	}).Return(nil)

	// Run both chains over the same segment
	liveErr := liveForwarder.ForwardSegment(utCtxt, sharedSegment, true)
	assert.Nil(recordingForwarder.ForwardSegment(utCtxt, testRecordings, []common.VideoSegmentWithData{sharedSegment}))

	// The live chain reports its failure honestly
	assert.Error(liveErr)
	mockLiveSender.AssertNumberOfCalls(t, "ForwardSegment", retryCfg.MaxAttempts)

	// The recording chain is unaffected: one upload, one report per session
	recorder.waitForReports(t, len(testRecordings))
	counts := recorder.countPerSession()
	for _, session := range testRecordings {
		assert.Equal(1, counts[session][sharedSegment.ID])
	}
	mockS3.AssertNumberOfCalls(t, "ForwardSegment", 1)

	assert.Nil(liveForwarder.Stop(utCtxt))
	assert.Nil(recordingForwarder.Stop(utCtxt))
}
