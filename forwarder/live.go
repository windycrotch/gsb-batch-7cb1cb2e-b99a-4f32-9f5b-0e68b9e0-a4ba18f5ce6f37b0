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
	"github.com/alwitt/livemix/db"
	"github.com/apex/log"
)

// ErrLiveSegmentForwardCancelled indicates a live segment's forwarding was cancelled
// before the segment was delivered to the control node (e.g. the source stopped
// streaming, or the forwarder was stopped while the segment was still in-flight).
// A cancelled segment was NOT delivered, and may be resubmitted later.
var ErrLiveSegmentForwardCancelled = errors.New("live segment forwarding cancelled before delivery")

// LiveStreamSegmentForwarder forward video segments in support of a live streaming
// a video source
type LiveStreamSegmentForwarder interface {
	/*
		Stop stop any background support tasks

		In-flight segments are given a chance to finish; segments which cannot
		complete are cancelled and their blocking callers are released with
		an error. Segments never report success without actual delivery.

			@param ctxt context.Context - execution context
	*/
	Stop(ctxt context.Context) error

	/*
		ForwardSegment process a new segment for forwarding

		The same segment (identified by source ID + segment name) is forwarded at
		most once: repeated submissions of an already delivered segment return
		success without re-sending, and repeated submissions of a segment which
		has exhausted its retries return the recorded failure.

			@param ctxt context.Context - execution context
			@param segment common.VideoSegmentWithData - segment to process
			@param blocking bool - block call until the forwarding complete
	*/
	ForwardSegment(ctxt context.Context, segment common.VideoSegmentWithData, blocking bool) error
}

// liveSegmentForwardState lifecycle states of a segment being forwarded
type liveSegmentForwardState int

const (
	// liveSegStateInFlight the segment is being forwarded right now
	liveSegStateInFlight liveSegmentForwardState = iota
	// liveSegStateDelivered the segment was delivered to the control node
	liveSegStateDelivered
	// liveSegStateFailed the segment exhausted its retries and is marked failed
	liveSegStateFailed
)

// liveSegmentTracking tracks the outcome of one segment's forwarding
type liveSegmentTracking struct {
	// state current lifecycle state
	state liveSegmentForwardState
	// result final outcome of the forwarding (valid once `done` is closed)
	result error
	// done closed exactly once when the segment leaves the in-flight state
	done chan struct{}
}

// httpLiveStreamSegmentForwarder HTTP version of LiveStreamSegmentForwarder
type httpLiveStreamSegmentForwarder struct {
	goutils.Component
	dbConns          db.ConnectionManager
	client           SegmentSender
	retryCfg         SegmentForwardRetryConfig
	workers          goutils.TaskProcessor
	wg               sync.WaitGroup
	workerCtxt       context.Context
	workerCtxtCancel context.CancelFunc
	// statusLock guards segmentStatus
	statusLock sync.Mutex
	// stopped whether Stop was called; new submissions are rejected afterwards
	stopped bool
	// segmentStatus per-segment forwarding outcome, keyed by source ID + segment name.
	// Terminal entries (delivered / failed) are kept so repeated submissions of the
	// same segment observe the same explainable result instead of being re-sent.
	segmentStatus map[string]*liveSegmentTracking
}

/*
NewHTTPLiveStreamSegmentForwarder define new HTTP version of LiveStreamSegmentForwarder

	@param parentCtxt context.Context - forwarder's parent execution context
	@param dbConns db.ConnectionManager - DB connection manager
	@param sender SegmentSender - client for forwarding video segments to system control node
	@param retryCfg SegmentForwardRetryConfig - retry configuration for segment forwarding
	@param maxInFlightSegments int - max number of segment being forwarded at any one time
	@param tpMetrics goutils.TaskProcessorMetricHelper - task processor metrics helper
	@returns new LiveStreamSegmentForwarder
*/
func NewHTTPLiveStreamSegmentForwarder(
	parentCtxt context.Context,
	dbConns db.ConnectionManager,
	sender SegmentSender,
	retryCfg SegmentForwardRetryConfig,
	maxInFlightSegments int,
	tpMetrics goutils.TaskProcessorMetricHelper,
) (LiveStreamSegmentForwarder, error) {
	logTags := log.Fields{
		"module":    "forwarder",
		"component": "http-segment-forwarder",
	}

	instance := &httpLiveStreamSegmentForwarder{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		dbConns:       dbConns,
		client:        sender,
		retryCfg:      retryCfg.normalized(),
		wg:            sync.WaitGroup{},
		segmentStatus: map[string]*liveSegmentTracking{},
	}

	// Worker context
	instance.workerCtxt, instance.workerCtxtCancel = context.WithCancel(parentCtxt)

	// Support worker
	workerLogsTags := log.Fields{
		"module":     "forwarder",
		"component":  "http-segment-forwarder",
		"sub-module": "support-worker",
	}
	workers, err := goutils.GetNewTaskDemuxProcessorInstance(
		instance.workerCtxt,
		"segment-forwarder-core",
		maxInFlightSegments+1,
		maxInFlightSegments+1,
		workerLogsTags,
		tpMetrics,
	)
	if err != nil {
		log.WithError(err).WithFields(logTags).Error("Failed to define support worker")
		return nil, err
	}
	instance.workers = workers

	// -----------------------------------------------------------------------------
	// Define support tasks

	if err := workers.AddToTaskExecutionMap(
		reflect.TypeOf(liveSegFwdForwardReq{}), instance.forwardSegment,
	); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to install task definition")
		return nil, err
	}

	// -----------------------------------------------------------------------------
	// Start the worker

	if err := workers.StartEventLoop(&instance.wg); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to start the worker threads")
		return nil, err
	}

	return instance, nil
}

func (f *httpLiveStreamSegmentForwarder) Stop(ctxt context.Context) error {
	logTags := f.GetLogTagsForContext(ctxt)

	// Reject new submissions first, so no segment can be accepted after this point
	// without its completion being tracked by the sweep below
	f.statusLock.Lock()
	f.stopped = true
	f.statusLock.Unlock()

	f.workerCtxtCancel()
	if err := f.workers.StopEventLoop(); err != nil {
		return err
	}
	if err := goutils.TimeBoundedWaitGroupWait(ctxt, &f.wg, time.Second*10); err != nil {
		return err
	}

	// Cancel any segment still tracked as in-flight. These are segments which were
	// accepted but never executed (e.g. still queued when the event loop stopped).
	// Their blocking callers must be released with an explicit cancellation rather
	// than being left hanging or, worse, believing the segment was delivered.
	f.statusLock.Lock()
	cancelled := 0
	for key, tracking := range f.segmentStatus {
		if tracking.state == liveSegStateInFlight {
			tracking.result = ErrLiveSegmentForwardCancelled
			close(tracking.done)
			delete(f.segmentStatus, key)
			cancelled++
		}
	}
	f.statusLock.Unlock()

	if cancelled > 0 {
		log.
			WithFields(logTags).
			WithField("cancelled-segments", cancelled).
			Warn("Cancelled in-flight segment forwards during stop")
	}
	return nil
}

// ======================================================================================
// Segment Processing

type liveSegFwdForwardReq struct {
	Key     string
	Segment common.VideoSegmentWithData
}

// liveSegmentStatusKey compute the deduplication key of a segment
func liveSegmentStatusKey(segment common.VideoSegmentWithData) string {
	return fmt.Sprintf("%s/%s", segment.SourceID, segment.Name)
}

func (f *httpLiveStreamSegmentForwarder) ForwardSegment(
	ctxt context.Context, segment common.VideoSegmentWithData, blocking bool,
) error {
	logTags := f.GetLogTagsForContext(ctxt)

	key := liveSegmentStatusKey(segment)

	// Check whether this segment was already seen, and reserve it otherwise
	f.statusLock.Lock()
	if f.stopped {
		f.statusLock.Unlock()
		return fmt.Errorf(
			"segment forwarder is stopped: %w", ErrLiveSegmentForwardCancelled,
		)
	}
	if tracking, ok := f.segmentStatus[key]; ok {
		switch tracking.state {
		case liveSegStateDelivered:
			f.statusLock.Unlock()
			log.
				WithFields(logTags).
				WithField("source-id", segment.SourceID).
				WithField("segment-name", segment.Name).
				Debug("Segment was already delivered; not forwarding again")
			// Already delivered; do not produce a duplicate success record
			return nil
		case liveSegStateFailed:
			result := tracking.result
			f.statusLock.Unlock()
			log.
				WithError(result).
				WithFields(logTags).
				WithField("source-id", segment.SourceID).
				WithField("segment-name", segment.Name).
				Warn("Segment was previously marked as failed; not retrying implicitly")
			return fmt.Errorf("segment previously marked as failed: %w", result)
		default:
			// Already in-flight; attach to the ongoing attempt
			done := tracking.done
			f.statusLock.Unlock()
			if !blocking {
				return nil
			}
			select {
			case <-ctxt.Done():
				return fmt.Errorf("context timed out before 'ForwardSegment' completed")
			case <-done:
				return tracking.result
			}
		}
	}
	tracking := &liveSegmentTracking{state: liveSegStateInFlight, done: make(chan struct{})}
	f.segmentStatus[key] = tracking
	f.statusLock.Unlock()

	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segment-name", segment.Name).
		Debug("Submitting new segment for processing")

	err := f.workers.Submit(ctxt, liveSegFwdForwardReq{Key: key, Segment: segment})
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segment-name", segment.Name).
			Error("Failed to submit new segment for processing")
		// The segment was never accepted for processing; cancel the tracking entry
		// so a later submission can try again.
		f.completeSegmentForward(key, ErrLiveSegmentForwardCancelled, false)
		return err
	}

	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segment-name", segment.Name).
		Debug("New segment submitted for processing")

	if blocking {
		select {
		case <-ctxt.Done():
			return fmt.Errorf("context timed out before 'ForwardSegment' completed")
		case <-tracking.done:
			return tracking.result
		}
	}

	return nil
}

/*
completeSegmentForward record the final outcome of one segment's forwarding.

	@param key string - the segment tracking key
	@param result error - the outcome; nil means delivered
	@param terminal bool - true to keep the outcome recorded (delivered / failed);
	    false to cancel the tracking, allowing future resubmissions
*/
func (f *httpLiveStreamSegmentForwarder) completeSegmentForward(
	key string, result error, terminal bool,
) {
	f.statusLock.Lock()
	defer f.statusLock.Unlock()

	tracking, ok := f.segmentStatus[key]
	if !ok || tracking.state != liveSegStateInFlight {
		// Already finalized (e.g. cancelled by Stop)
		return
	}
	if terminal {
		if result == nil {
			tracking.state = liveSegStateDelivered
		} else {
			tracking.state = liveSegStateFailed
		}
	} else {
		// Cancelled: forget the segment so it can be resubmitted later
		delete(f.segmentStatus, key)
	}
	tracking.result = result
	close(tracking.done)
}

func (f *httpLiveStreamSegmentForwarder) forwardSegment(params interface{}) error {
	// Convert params into expected data type
	if request, ok := params.(liveSegFwdForwardReq); ok {
		return f.handleForwardSegment(request)
	}
	err := fmt.Errorf("received unexpected call parameters: %s", reflect.TypeOf(params))
	logTags := f.GetLogTagsForContext(f.workerCtxt)
	log.WithError(err).WithFields(logTags).Error("'ForwardSegment' processing failure")
	return err
}

func (f *httpLiveStreamSegmentForwarder) handleForwardSegment(
	request liveSegFwdForwardReq,
) error {
	logTags := f.GetLogTagsForContext(f.workerCtxt)
	segment := request.Segment

	// Abort early if the forwarder is stopping
	if err := f.workerCtxt.Err(); err != nil {
		f.completeSegmentForward(request.Key, ErrLiveSegmentForwardCancelled, false)
		return err
	}

	dbClient := f.dbConns.NewPersistanceManager()
	defer dbClient.Close()

	// Verify forwarding is enabled first
	source, err := dbClient.GetVideoSource(f.workerCtxt, segment.SourceID)
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segment-name", segment.Name).
			Error("Unable to find the video source entry in persistence")
		f.completeSegmentForward(request.Key, err, true)
		return err
	}
	if source.Streaming != 1 {
		log.
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segment-name", segment.Name).
			Debug("Video source is not instructed to forward video segments")
		// The segment will NOT be delivered. Report an explicit cancellation rather
		// than a success, so no caller believes the segment reached the control node.
		f.completeSegmentForward(request.Key, ErrLiveSegmentForwardCancelled, false)
		return ErrLiveSegmentForwardCancelled
	}

	// Forward the segment with bounded retries
	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segment-name", segment.Name).
		Debug("Sending out segment")
	err = runSegmentOpWithRetry(f.workerCtxt, f.retryCfg, func() error {
		return f.client.ForwardSegment(f.workerCtxt, segment)
	})
	if err != nil {
		if f.workerCtxt.Err() != nil {
			// Aborted by shutdown, not by a delivery failure. The segment was not
			// delivered; cancel it so it does not stay marked as a hard failure.
			log.
				WithError(err).
				WithFields(logTags).
				WithField("source-id", segment.SourceID).
				WithField("segment-name", segment.Name).
				Warn("Segment forwarding cancelled before delivery")
			f.completeSegmentForward(request.Key, ErrLiveSegmentForwardCancelled, false)
			return err
		}
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segment-name", segment.Name).
			Error("Failed to send out video segment; retries exhausted")
		// Mark the segment as explicitly failed. Future submissions of the same
		// segment observe this failure instead of creating duplicate records.
		f.completeSegmentForward(request.Key, err, true)
		return err
	}
	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segment-name", segment.Name).
		Debug("Video segment forwarded")
	f.completeSegmentForward(request.Key, nil, true)
	return nil
}
