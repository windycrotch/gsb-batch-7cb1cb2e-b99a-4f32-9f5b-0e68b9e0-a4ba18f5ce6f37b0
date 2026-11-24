package forwarder

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/alwitt/goutils"
	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/common/ipc"
	"github.com/alwitt/livemix/utils"
	"github.com/apex/log"
)

// ErrRecordingSegmentForwardCancelled indicates a recording segment's store-and-report
// pipeline was cancelled before completion (e.g. the forwarder was stopped while the
// segment was still in-flight). A cancelled segment may be resubmitted later.
var ErrRecordingSegmentForwardCancelled = errors.New(
	"recording segment forwarding cancelled before completion",
)

// RecordingSegmentForwarder forward video segments associated with active recording sessions
type RecordingSegmentForwarder interface {
	/*
		Stop stop any background support tasks

		In-flight segment writes either complete (and report), or are cancelled
		without leaving half-completed state behind.

			@param ctxt context.Context - execution context
	*/
	Stop(ctxt context.Context) error

	/*
		ForwardSegment process new segments associated with active video recording sessions

		A segment is stored to the object store before any "segment stored" event is
		broadcast. Each recording session receives its own report per segment, and a
		given (session, segment) pair is reported at most once even across repeated
		submissions.

			@param ctxt context.Context - execution context
			@param recordings []string - set of recording session IDs
			@param segments []common.VideoSegmentWithData - new video segments to report
	*/
	ForwardSegment(
		ctxt context.Context, recordings []string, segments []common.VideoSegmentWithData,
	) error
}

// recordingSegmentTracking tracks the store-and-report state of one segment
type recordingSegmentTracking struct {
	// uploaded whether the segment content is known to be in the object store
	uploaded bool
	// uploadInFlight whether some task is currently uploading the segment
	uploadInFlight bool
	// uploadDone closed when the in-flight upload attempt concludes
	uploadDone chan struct{}
	// reported recording session IDs whose "segment stored" report was emitted
	reported map[string]bool
	// pending recording session IDs reserved by an in-flight task
	pending map[string]bool
	// failed the terminal failure of the last processing attempt, if any
	failed error
}

// s3RecordingSegmentForwarder S3 version of RecordingSegmentForwarder
type s3RecordingSegmentForwarder struct {
	goutils.Component
	storageCfg       common.RecordingStorageConfig
	s3Client         SegmentSender
	broadcastClient  utils.Broadcaster
	retryCfg         SegmentForwardRetryConfig
	txWorkers        goutils.TaskProcessor
	wg               sync.WaitGroup
	workerCtxt       context.Context
	workerCtxtCancel context.CancelFunc
	// statusLock guards segmentStatus
	statusLock sync.Mutex
	// stopped whether Stop was called; new submissions are rejected afterwards
	stopped bool
	// segmentStatus per-segment store-and-report state, keyed by segment ID
	segmentStatus map[string]*recordingSegmentTracking
}

/*
NewS3RecordingSegmentForwarder define new S3 version of RecordingSegmentForwarder

	@param parentCtxt context.Context - forwarder's parent execution context
	@param storageCfg common.RecordingStorageConfig - segment storage config
	@param s3Client SegmentSender - S3 segment transport client
	@param broadcastClient utils.Broadcaster - message broadcast client
	@param retryCfg SegmentForwardRetryConfig - retry configuration for segment storage
	@param maxInFlightSegments int - max number of segment being stored at any one time
	@param tpMetrics goutils.TaskProcessorMetricHelper - task processor metrics helper
	@returns new RecordingSegmentForwarder
*/
func NewS3RecordingSegmentForwarder(
	parentCtxt context.Context,
	storageCfg common.RecordingStorageConfig,
	s3Client SegmentSender,
	broadcastClient utils.Broadcaster,
	retryCfg SegmentForwardRetryConfig,
	maxInFlightSegments int,
	tpMetrics goutils.TaskProcessorMetricHelper,
) (RecordingSegmentForwarder, error) {
	logTags := log.Fields{
		"module":    "forwarder",
		"component": "s3-recording-segment-forwarder",
	}

	instance := &s3RecordingSegmentForwarder{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		storageCfg:      storageCfg,
		s3Client:        s3Client,
		broadcastClient: broadcastClient,
		retryCfg:        retryCfg.normalized(),
		wg:              sync.WaitGroup{},
		segmentStatus:   map[string]*recordingSegmentTracking{},
	}

	// Worker context
	instance.workerCtxt, instance.workerCtxtCancel = context.WithCancel(parentCtxt)

	// -----------------------------------------------------------------------------
	// Prepare TX workers
	txWorkerLogTags := log.Fields{
		"module":     "forwarder",
		"component":  "s3-recording-segment-forwarder",
		"sub-module": "transmit-worker",
	}
	workers, err := goutils.GetNewTaskDemuxProcessorInstance(
		instance.workerCtxt,
		"segment-forwarder-transmit",
		maxInFlightSegments+1,
		maxInFlightSegments+1,
		txWorkerLogTags,
		tpMetrics,
	)
	if err != nil {
		log.WithError(err).WithFields(logTags).Error("Failed to define transmission worker")
		return nil, err
	}
	instance.txWorkers = workers

	// Define support tasks
	if err := workers.AddToTaskExecutionMap(
		reflect.TypeOf(recordingSegmentForwardRequest{}), instance.forwardSegment,
	); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to install task definition")
		return nil, err
	}

	// -----------------------------------------------------------------------------
	// Start the worker

	if err := workers.StartEventLoop(&instance.wg); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to start the transmission worker threads")
		return nil, err
	}

	return instance, nil
}

func (f *s3RecordingSegmentForwarder) Stop(ctxt context.Context) error {
	logTags := f.GetLogTagsForContext(ctxt)

	// Reject new submissions first, so no segment can be accepted after this point
	// without its completion being tracked by the sweep below
	f.statusLock.Lock()
	f.stopped = true
	f.statusLock.Unlock()

	f.workerCtxtCancel()
	if err := f.txWorkers.StopEventLoop(); err != nil {
		return err
	}
	if err := goutils.TimeBoundedWaitGroupWait(ctxt, &f.wg, time.Second*10); err != nil {
		return err
	}

	// Any segment still holding pending (reserved but unreported) sessions was
	// accepted but not completed; release the reservations so the state left behind
	// does not look half-completed, and log the cancellation for explainability.
	f.statusLock.Lock()
	cancelled := 0
	for _, tracking := range f.segmentStatus {
		cancelled += len(tracking.pending)
		tracking.pending = map[string]bool{}
	}
	f.statusLock.Unlock()

	if cancelled > 0 {
		log.
			WithFields(logTags).
			WithField("cancelled-reports", cancelled).
			Warn("Cancelled in-flight recording segment reports during stop")
	}
	return nil
}

// ======================================================================================
// Segment Processing

type recordingSegmentForwardRequest struct {
	key       string
	recording []string
	segment   common.VideoSegmentWithData
}

// recordingSegmentStatusKey compute the deduplication key of a recording segment
func recordingSegmentStatusKey(segment common.VideoSegmentWithData) string {
	if segment.ID != "" {
		return segment.ID
	}
	return fmt.Sprintf("%s/%s", segment.SourceID, segment.Name)
}

func (f *s3RecordingSegmentForwarder) ForwardSegment(
	ctxt context.Context, recordings []string, segments []common.VideoSegmentWithData,
) error {
	logTags := f.GetLogTagsForContext(ctxt)

	var firstErr error

	// Process each segment separately
	for _, oneSegment := range segments {
		// Define segment storage URI. This tells the sender where to place the object
		oneSegment.URI = fmt.Sprintf(
			"s3://%s/%s/%s/%s",
			f.storageCfg.StorageBucket,
			f.storageCfg.StorageObjectPrefix,
			oneSegment.SourceID,
			oneSegment.Name,
		)
		key := recordingSegmentStatusKey(oneSegment)

		// Reserve the (session, segment) reports which still need to be emitted.
		// Sessions already reported, or reserved by an in-flight task, are skipped
		// so a session never receives duplicate reports for the same segment.
		f.statusLock.Lock()
		if f.stopped {
			// Checked under the same lock as the reservation, so no reservation
			// can slip in after Stop began rejecting submissions
			f.statusLock.Unlock()
			if firstErr == nil {
				firstErr = fmt.Errorf(
					"segment forwarder is stopped: %w", ErrRecordingSegmentForwardCancelled,
				)
			}
			continue
		}
		tracking, ok := f.segmentStatus[key]
		if !ok {
			tracking = &recordingSegmentTracking{
				reported: map[string]bool{}, pending: map[string]bool{},
			}
			f.segmentStatus[key] = tracking
		}
		if oneSegment.Uploaded != nil {
			// Segment content is already in the object store according to persistence
			tracking.uploaded = true
		}
		// A new submission gives a previously failed segment another chance
		tracking.failed = nil
		pendingSessions := []string{}
		for _, session := range recordings {
			if tracking.reported[session] || tracking.pending[session] {
				continue
			}
			tracking.pending[session] = true
			pendingSessions = append(pendingSessions, session)
		}
		// Skip only when there is nothing new to report and the upload is already
		// done or being handled by another task
		uploadHandled := tracking.uploaded || tracking.uploadInFlight
		f.statusLock.Unlock()

		if len(pendingSessions) == 0 && uploadHandled {
			log.
				WithFields(logTags).
				WithField("source-id", oneSegment.SourceID).
				WithField("segment", oneSegment.Name).
				Debug("Segment already stored and reported for all sessions; skipping")
			continue
		}

		log.
			WithFields(logTags).
			WithField("source-id", oneSegment.SourceID).
			WithField("segment", oneSegment.Name).
			WithField("storage-uri", oneSegment.URI).
			WithField("sessions-to-report", len(pendingSessions)).
			Debug("Submitting new recording segment forward request")

		request := recordingSegmentForwardRequest{
			key: key, recording: pendingSessions, segment: oneSegment,
		}
		if err := f.txWorkers.Submit(ctxt, request); err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				WithField("source-id", oneSegment.SourceID).
				WithField("segment", oneSegment.Name).
				Error("Failed to submit a new recording segment forward request")
			// Release the reservations; the segment was never accepted for processing
			f.releaseSessionReservations(key, pendingSessions)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}

	log.
		WithFields(logTags).
		WithField("segments-to-record", len(segments)).
		Debug("Processed all new recording segment forward requests")

	return firstErr
}

/*
releaseSessionReservations drop the in-flight reservations of the given sessions,
so a future submission can pick the work up again.

	@param key string - the segment tracking key
	@param sessions []string - the recording session IDs to release
*/
func (f *s3RecordingSegmentForwarder) releaseSessionReservations(
	key string, sessions []string,
) {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()
	tracking, ok := f.segmentStatus[key]
	if !ok {
		return
	}
	for _, session := range sessions {
		delete(tracking.pending, session)
	}
}

func (f *s3RecordingSegmentForwarder) forwardSegment(params interface{}) error {
	if request, ok := params.(recordingSegmentForwardRequest); ok {
		return f.handleForwardSegment(request)
	}
	err := fmt.Errorf("received unexpected call parameters: %s", reflect.TypeOf(params))
	logTags := f.GetLogTagsForContext(f.workerCtxt)
	log.WithError(err).WithFields(logTags).Error("'ForwardSegment' processing failure")
	return err
}

/*
ensureSegmentUploaded make sure the segment content is in the object store.

If another task is already uploading the same segment, this task waits for that
upload to conclude instead of uploading a second copy. Returns nil once the
segment is known to be stored.

	@param param recordingSegmentForwardRequest - the segment being processed
	@returns nil once the segment is stored, or the failure reason
*/
func (f *s3RecordingSegmentForwarder) ensureSegmentUploaded(
	param recordingSegmentForwardRequest,
) error {
	logTags := f.GetLogTagsForContext(f.workerCtxt)

	for {
		f.statusLock.Lock()
		tracking, ok := f.segmentStatus[param.key]
		if !ok {
			// Tracking entry vanished (should not happen); treat as uploaded to
			// avoid uploading without any state tracking
			f.statusLock.Unlock()
			return nil
		}
		if tracking.uploaded {
			f.statusLock.Unlock()
			return nil
		}
		if tracking.uploadInFlight {
			// Another task is uploading right now; wait for its conclusion
			uploadDone := tracking.uploadDone
			f.statusLock.Unlock()
			select {
			case <-f.workerCtxt.Done():
				return ErrRecordingSegmentForwardCancelled
			case <-uploadDone:
			}
			continue
		}
		// Become the uploader for this segment
		tracking.uploadInFlight = true
		tracking.uploadDone = make(chan struct{})
		f.statusLock.Unlock()
		break
	}

	log.
		WithFields(logTags).
		WithField("source-id", param.segment.SourceID).
		WithField("segment", param.segment.Name).
		WithField("storage-uri", param.segment.URI).
		Debug("Storing recording segment")

	uploadErr := runSegmentOpWithRetry(f.workerCtxt, f.retryCfg, func() error {
		return f.s3Client.ForwardSegment(f.workerCtxt, param.segment)
	})

	cancelled := f.workerCtxt.Err() != nil

	f.statusLock.Lock()
	if tracking, ok := f.segmentStatus[param.key]; ok {
		if uploadErr == nil {
			tracking.uploaded = true
			tracking.failed = nil
		} else if !cancelled {
			tracking.failed = uploadErr
		}
		tracking.uploadInFlight = false
		close(tracking.uploadDone)
		tracking.uploadDone = nil
	}
	f.statusLock.Unlock()

	if uploadErr != nil {
		log.
			WithError(uploadErr).
			WithFields(logTags).
			WithField("source-id", param.segment.SourceID).
			WithField("segment", param.segment.Name).
			WithField("storage-uri", param.segment.URI).
			Error("Unable to forward recording segment")
		if cancelled {
			return ErrRecordingSegmentForwardCancelled
		}
		return uploadErr
	}

	log.
		WithFields(logTags).
		WithField("source-id", param.segment.SourceID).
		WithField("segment", param.segment.Name).
		WithField("storage-uri", param.segment.URI).
		Debug("Recording segment stored")
	return nil
}

func (f *s3RecordingSegmentForwarder) handleForwardSegment(
	param recordingSegmentForwardRequest,
) error {
	logTags := f.GetLogTagsForContext(f.workerCtxt)

	// Abort early if the forwarder is stopping
	if err := f.workerCtxt.Err(); err != nil {
		f.releaseSessionReservations(param.key, param.recording)
		return err
	}

	// Step 1: ensure the segment content is in the object store. No "segment stored"
	// event may be broadcast before this succeeds. Concurrent tasks for the same
	// segment coordinate through the tracking entry so the upload happens once.
	if err := f.ensureSegmentUploaded(param); err != nil {
		f.releaseSessionReservations(param.key, param.recording)
		return err
	}

	// Step 2: now that the segment is stored, emit one "segment stored" report per
	// recording session. A session is marked reported only after its broadcast
	// succeeds, so failures can be retried without duplicating successful reports.
	for _, session := range param.recording {
		if err := f.workerCtxt.Err(); err != nil {
			f.releaseSessionReservations(param.key, param.recording)
			return ErrRecordingSegmentForwardCancelled
		}

		report := ipc.NewRecordingSegmentReport(
			[]string{session}, []common.VideoSegment{param.segment.VideoSegment},
		)
		if err := f.broadcastClient.Broadcast(f.workerCtxt, &report); err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				WithField("source-id", param.segment.SourceID).
				WithField("segment", param.segment.Name).
				WithField("storage-uri", param.segment.URI).
				WithField("recording-id", session).
				Error("Unable to broadcast recording segment stored event")
			// Release this and the remaining sessions so they can be retried by a
			// future submission; already reported sessions stay reported.
			remaining := []string{}
			seen := false
			for _, s := range param.recording {
				if s == session {
					seen = true
				}
				if seen {
					remaining = append(remaining, s)
				}
			}
			f.releaseSessionReservations(param.key, remaining)
			return err
		}

		f.statusLock.Lock()
		if tracking, ok := f.segmentStatus[param.key]; ok {
			tracking.reported[session] = true
			delete(tracking.pending, session)
		}
		f.statusLock.Unlock()
	}

	return nil
}
