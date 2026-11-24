package control

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"sync"
	"time"

	"github.com/Glimesh/waveguide/config"
	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/orchestrator"
	"github.com/Glimesh/waveguide/pkg/service"
	"github.com/Glimesh/waveguide/pkg/types"
	"github.com/pion/rtp"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Pipe struct {
	Input        string
	Output       string
	Orchestrator string
}

// ingestRunner abstracts the blocking media ingest of a single stream. It is
// a field (rather than a hard-coded call to Stream.Ingest) so the teardown
// behaviour of the control plane can be exercised without standing up a real
// WebRTC peer connection.
type ingestRunner func(stream *Stream, ctx context.Context) error

// heartbeatFailureThreshold is the number of consecutive failed heartbeat
// ticks before a stream is torn down.
const heartbeatFailureThreshold = 5

type Control struct {
	ctx          context.Context
	service      service.Service
	orchestrator orchestrator.Orchestrator

	// stateMu protects streams and metadataCollectors. All reads and writes of
	// those maps must hold this lock (RLock for reads, Lock for writes).
	stateMu            sync.RWMutex
	streams            map[types.ChannelID]*Stream
	metadataCollectors map[types.ChannelID]chan bool
	teardownInProgress map[types.ChannelID]chan struct{}
	teardownMu         sync.Mutex

	log     logrus.FieldLogger
	httpMux *http.ServeMux

	// heartbeatInterval is how often a stream reports metadata while live.
	heartbeatInterval time.Duration
	// runIngest starts the (blocking) media ingest for a stream.
	runIngest ingestRunner

	Hostname       string
	HTTPServerType string `mapstructure:"http_server_type"`
	HTTPAddress    string `mapstructure:"http_address"`
	HTTPS          bool
	HTTPSHostname  string `mapstructure:"https_hostname"`
	HTTPSCert      string `mapstructure:"https_cert"`
	HTTPSKey       string `mapstructure:"https_key"`

	// Flag to enable saving video stream to file
	// Currently it's global flag toggled from the config file
	SaveVideo bool `mapstructure:"save_video"`
}

func New(
	ctx context.Context,
	cfg config.Config,
	hostname string,
	logger *logrus.Logger,
) (*Control, error) {
	svc := service.New(cfg, logger)
	if err := svc.Connect(); err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}

	or := orchestrator.New(cfg, hostname, logger)
	if err := or.Connect(); err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	httpCfg := cfg.Control

	ctrl := &Control{
		ctx:          ctx,
		service:      svc,
		orchestrator: or,

		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan bool),
		teardownInProgress: make(map[types.ChannelID]chan struct{}),
		httpMux:            http.NewServeMux(),
		log: logger.WithFields(logrus.Fields{
			"control": "waveguide",
		}),

		heartbeatInterval: 15 * time.Second,
		runIngest:         func(stream *Stream, ingestCtx context.Context) error { return stream.Ingest(ingestCtx) },

		Hostname:       hostname,
		HTTPServerType: httpCfg.HTTPServerType,
		HTTPAddress:    httpCfg.Address,
		HTTPSHostname:  httpCfg.HTTPSHostname,
		HTTPSCert:      httpCfg.HTTPSCert,
		HTTPSKey:       httpCfg.HTTPSKey,

		// this should be controlled at a stream level
		SaveVideo: cfg.Control.SaveVideo,
	}

	return ctrl, nil
}

// newControlWithDeps builds a Control backed by caller-supplied (typically fake)
// service and orchestrator. It exists so the teardown state machine can be
// unit tested without opening real network connections. Production code uses
// New.
func newControlWithDeps(
	ctx context.Context,
	svc service.Service,
	or orchestrator.Orchestrator,
	logger *logrus.Logger,
) *Control {
	ctrl := &Control{
		ctx:          ctx,
		service:      svc,
		orchestrator: or,

		streams:            make(map[types.ChannelID]*Stream),
		metadataCollectors: make(map[types.ChannelID]chan bool),
		teardownInProgress: make(map[types.ChannelID]chan struct{}),
		httpMux:            http.NewServeMux(),
		log:                logger.WithField("control", "waveguide-test"),

		heartbeatInterval: 15 * time.Second,
		runIngest:         func(stream *Stream, ingestCtx context.Context) error { return stream.Ingest(ingestCtx) },
	}

	return ctrl
}

func (ctrl *Control) Context() context.Context {
	return ctrl.ctx
}

func (ctrl *Control) ContextErr() error {
	return ctrl.Context().Err()
}

// Shutdown performs the same idempotent teardown as StopStream on every
// stream that is still live when the server stops. Each channel is still
// torn down at most once and each external service receives at most one end
// notification.
func (ctrl *Control) Shutdown() {
	ctrl.stateMu.RLock()
	channelIDs := make([]types.ChannelID, 0, len(ctrl.streams))
	for channelID := range ctrl.streams {
		channelIDs = append(channelIDs, channelID)
	}
	ctrl.stateMu.RUnlock()

	for _, channelID := range channelIDs {
		if err := ctrl.StopStream(channelID); err != nil {
			ctrl.log.WithError(err).WithField("channel_id", channelID).
				Error("error tearing down stream during shutdown")
		}
	}
}

func (ctrl *Control) GetTracks(channelID types.ChannelID) ([]StreamTrack, error) {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return nil, err
	}

	return stream.tracks, nil
}

func (ctrl *Control) GetHmacKey(channelID types.ChannelID) (string, error) {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return "", err
	}

	return string(actualKey), nil
}

func (ctrl *Control) Authenticate(channelID types.ChannelID, streamKey types.StreamKey) error {
	actualKey, err := ctrl.service.GetHmacKey(channelID)
	if err != nil {
		return err
	}
	if string(streamKey) != string(actualKey) {
		return errors.New("incorrect stream key")
	}

	return nil
}

func (ctrl *Control) StartStream(channelID types.ChannelID) (*Stream, error) {
	ctx, cancel := context.WithCancel(ctrl.Context())

	stream, err := ctrl.newStream(channelID, cancel)
	if err != nil {
		cancel()
		return nil, err
	}

	ctrl.log.Infof("Starting stream for %s", channelID)

	streamID, err := ctrl.service.StartStream(channelID)
	if err != nil {
		ctrl.removeStream(channelID)
		cancel()
		return nil, err
	}
	stream.SetStreamID(streamID)

	err = ctrl.orchestrator.StartStream(stream.ChannelID, stream.StreamID())
	if err != nil {
		// Run the full teardown so the external service is told the stream
		// ended and the local state is cleaned up.
		ctrl.StopStream(channelID)
		return nil, err
	}

	go ctrl.setupHeartbeat(channelID)

	go func() {
		// A returning ingest (publisher disconnect, read error, ...) funnels
		// through the same idempotent StopStream path as every other trigger.
		if ingestErr := ctrl.runIngest(stream, ctx); ingestErr != nil { //nolint not shadowed
			stream.log.Error(ingestErr)
		}
		ctrl.StopStream(channelID)
	}()

	return stream, nil
}

// StopStream converges every possible end-of-stream trigger (publisher
// disconnect, WHEP/WHIP delete, heartbeat failure, server shutdown) onto a
// single teardown per channel.
//
// Guarantees:
//   - Calling it more than once for the same channel (including concurrent
//     calls while the first teardown is still running) never returns an error
//     caused by the stream being gone.
//   - The external service's EndStream and the orchestrator's StopStream are
//     each invoked at most once per channel.
//   - The stream is always removed from the local active-stream table, even if
//     an external call fails, so the channel can immediately go live again.
//   - External failures are logged (observable) rather than swallowed; the
//     first caller that actually performs the teardown also receives the error.
func (ctrl *Control) StopStream(channelID types.ChannelID) error {
	done, isTeardownOwner := ctrl.beginTeardown(channelID)
	if done == nil {
		// A teardown has already fully completed: there is nothing left to do
		// and nothing to report to a duplicate caller.
		return nil
	}

	if !isTeardownOwner {
		// Another caller is already tearing this channel down. Wait for it so
		// that callers which depend on the stream being fully gone observe the
		// completed state, but do not perform (or report) the work twice.
		<-done
		return nil
	}

	teardownErr := ctrl.teardownStream(channelID)
	ctrl.finishTeardown(channelID, done)

	return teardownErr
}

// beginTeardown claims the right to tear a channel down.
//
// Returns:
//   - (nil, false): teardown for this channel already completed previously.
//   - (done, false): a teardown is in progress owned by another caller; wait
//     on done.
//   - (done, true): this caller owns the teardown and must run teardownStream
//     and then finishTeardown.
func (ctrl *Control) beginTeardown(channelID types.ChannelID) (chan struct{}, bool) {
	ctrl.teardownMu.Lock()
	defer ctrl.teardownMu.Unlock()

	if done, inProgress := ctrl.teardownInProgress[channelID]; inProgress {
		return done, false
	}

	ctrl.stateMu.RLock()
	_, exists := ctrl.streams[channelID]
	ctrl.stateMu.RUnlock()
	if !exists {
		// Either never existed or a previous teardown already removed it.
		return nil, false
	}

	done := make(chan struct{})
	ctrl.teardownInProgress[channelID] = done

	return done, true
}

// finishTeardown marks the channel's teardown as complete and wakes any
// concurrent callers waiting in StopStream.
func (ctrl *Control) finishTeardown(channelID types.ChannelID, done chan struct{}) {
	ctrl.teardownMu.Lock()
	delete(ctrl.teardownInProgress, channelID)
	ctrl.teardownMu.Unlock()

	close(done)
}

// teardownStream performs the actual, exactly-once end-of-stream work. It must
// only be invoked by the caller that claimed ownership via beginTeardown.
func (ctrl *Control) teardownStream(channelID types.ChannelID) error {
	ctrl.log.Debug("Stop Stream")

	stream, err := ctrl.getStream(channelID)
	if err != nil {
		// Ownership was claimed while the stream existed, so this should not
		// happen; treat it as an already-completed teardown.
		return nil
	}

	// 1. Stop the local stream (cancel ingest context). Idempotent under
	//    repeated Stop calls.
	stream.Stop()

	// 2. Tell the metadata collector to stop. Non-blocking: the collector may
	//    already have exited, and there is nothing to wait on here.
	ctrl.stateMu.RLock()
	collector, hasCollector := ctrl.metadataCollectors[channelID]
	ctrl.stateMu.RUnlock()
	if hasCollector {
		select {
		case collector <- true:
			ctrl.log.Debug("sent metadata collector signal")
		default:
			ctrl.log.Debug("metadata collector already stopping")
		}
	}

	// 3. Notify external systems. These run before the local state is removed,
	//    but a failure must NOT prevent local cleanup, otherwise the channel
	//    would stay wedged as "live" and reject the next publish.
	serviceErr := ctrl.service.EndStream(stream.StreamID())
	if serviceErr != nil {
		stream.log.Error(errors.Wrap(serviceErr, "error ending stream on service"))
	}
	orchestratorErr := ctrl.orchestrator.StopStream(stream.ChannelID, stream.StreamID())
	if orchestratorErr != nil {
		stream.log.Error(errors.Wrap(orchestratorErr, "error stopping stream on orchestrator"))
	}

	// 4. Always remove the stream from the local active-stream table so the
	//    channel returns to a clean, re-publishable state.
	if removeErr := ctrl.removeStream(channelID); removeErr != nil {
		stream.log.Error(removeErr)
	}

	// Return the first external failure to the owning caller. Duplicate
	// callers never reach here.
	if serviceErr != nil {
		return serviceErr
	}

	return orchestratorErr
}

var (
	ErrHeartbeatThumbnail             = errors.New("error sending thumbnail")
	ErrHeartbeatSendMetadata          = errors.New("error sending metadata")
	ErrHeartbeatOrchestratorHeartbeat = errors.New("error sending orchestrator heartbeat")
)

func (ctrl *Control) setupHeartbeat(channelID types.ChannelID) {
	ticker := time.NewTicker(ctrl.heartbeatInterval)
	tickFailed := 0

	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return
	}

	// Capture the stop channel up front. The channel is deleted from
	// metadataCollectors during teardown; reading from a captured reference
	// keeps this goroutine safe (and leak-free) when it is the one triggering
	// that teardown.
	ctrl.stateMu.RLock()
	stopCollector := ctrl.metadataCollectors[channelID]
	ctrl.stateMu.RUnlock()
	if stopCollector == nil {
		ticker.Stop()
		return
	}

	for {
		select {
		case <-ticker.C:
			stream.log.Infof("Collecting metadata tickFailed=%d", tickFailed)
			var err error
			hasErrors := false

			err = ctrl.sendThumbnail(channelID)
			if err != nil {
				stream.log.Error(errors.Wrap(err, ErrHeartbeatThumbnail.Error()))
				hasErrors = true
			}

			err = ctrl.sendMetadata(channelID)
			if err != nil {
				stream.log.Error(errors.Wrap(err, ErrHeartbeatSendMetadata.Error()))
				hasErrors = true
			}

			err = ctrl.orchestrator.Heartbeat(channelID)
			if err != nil {
				stream.log.Error(errors.Wrap(err, ErrHeartbeatOrchestratorHeartbeat.Error()))
				hasErrors = true
			}

			if hasErrors {
				tickFailed++
			} else if tickFailed > 0 {
				tickFailed--
			}

			// Look for several consecutive failures and tear the stream down
			// through the same path as a publisher disconnect, so the external
			// service is notified and the local state is cleaned up exactly once.
			if tickFailed >= heartbeatFailureThreshold {
				stream.log.Warn("Stopping stream due to excessive heartbeat errors")
				ticker.Stop()
				if stopErr := ctrl.StopStream(channelID); stopErr != nil {
					stream.log.Error(stopErr)
				}
				return
			}

		case <-stopCollector:
			ticker.Stop()
			return
		}
	}
}

func (ctrl *Control) sendMetadata(channelID types.ChannelID) error {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return err
	}

	stream.lastTime = time.Now().Unix()

	return ctrl.service.UpdateStreamMetadata(stream.StreamID(), types.StreamMetadata{
		AudioCodec:        stream.audioCodec,
		IngestServer:      ctrl.Hostname,
		IngestViewers:     0,
		LostPackets:       0, // Don't exist
		NackPackets:       0, // Don't exist
		RecvPackets:       stream.totalAudioPackets + stream.totalVideoPackets,
		SourceBitrate:     0, // Likely just need to calculate the bytes between two 5s snapshots?
		SourcePing:        0, // Not accessible unless we ping them manually
		StreamTimeSeconds: int(stream.lastTime - stream.startTime),
		VendorName:        stream.clientVendorName,
		VendorVersion:     stream.clientVendorVersion,
		VideoCodec:        stream.videoCodec,
		VideoHeight:       stream.videoHeight,
		VideoWidth:        stream.videoWidth,
	})
}

func (ctrl *Control) sendThumbnail(channelID types.ChannelID) (err error) {
	stream, err := ctrl.getStream(channelID)
	if err != nil {
		return err
	}

	// signal the stream thumnailer to get me some thumbnails
	select {
	case stream.requestThumbnail <- struct{}{}:
	default:
	}

	ctrl.log.Debug("thumbnail requested")

	var data []byte
	// Since stream.lastThumbnail is a buffered chan, let's read all values to get the newest
	for len(stream.lastThumbnail) > 0 {
		data = <-stream.lastThumbnail
	}

	if len(data) == 0 {
		return nil
	}

	var img image.Image
	h264dec, err := newImageDecoder()
	if err != nil {
		return err
	}
	defer h264dec.Close()
	img, err = h264dec.Decode(data)
	if err != nil {
		return err
	}
	if img == nil {
		ctrl.log.WithField("channel_id", channelID).Debug("img is nil")
		return nil
	}

	buff := new(bytes.Buffer)
	err = jpeg.Encode(buff, img, &jpeg.Options{
		Quality: 75,
	})
	if err != nil {
		return err
	}

	err = ctrl.service.SendJpegPreviewImage(stream.StreamID(), buff.Bytes())
	if err != nil {
		return err
	}

	ctrl.log.WithField("channel_id", channelID).Debug("Got screenshot!")

	// Also update our metadata
	stream.videoWidth = img.Bounds().Dx()
	stream.videoHeight = img.Bounds().Dy()

	return nil
}

func (ctrl *Control) newStream(channelID types.ChannelID, cancelFunc context.CancelFunc) (*Stream, error) {
	ctrl.stateMu.Lock()
	defer ctrl.stateMu.Unlock()

	if _, exists := ctrl.streams[channelID]; exists {
		return nil, errors.New("stream already exists in stream manager state")
	}

	stream := &Stream{ //nolint exhaustive struct
		ChannelID: channelID,

		log:           ctrl.log.WithField("channel_id", channelID),
		whepURI:       ctrl.HTTPServerURL() + "/whep/endpoint/" + channelID.String(),
		authenticated: true,

		cancelFunc:        cancelFunc,
		kf:                keyframer.New(),
		rtpIngest:         make(chan *rtp.Packet),
		stopThumbnailer:   make(chan struct{}, 1),
		thumbnailReceiver: make(chan *rtp.Packet, 50),
		requestThumbnail:  make(chan struct{}, 1),

		lastThumbnail: make(chan []byte, 1),

		startTime: time.Now().Unix(),
	}

	// TODO: this shouldn't be a global flag
	// but rather configured on per stream basis
	if ctrl.SaveVideo {
		stream.saveVideo = true
		stream.videoWriterChan = make(chan *rtp.Packet, 100) // not sure what the buffer size here should be
	}

	ctrl.streams[channelID] = stream
	ctrl.metadataCollectors[channelID] = make(chan bool, 1)

	return stream, nil
}

func (ctrl *Control) removeStream(id types.ChannelID) error {
	ctrl.stateMu.Lock()
	defer ctrl.stateMu.Unlock()

	if _, exists := ctrl.streams[id]; !exists {
		return errors.New("RemoveStream stream does not exist in state")
	}

	delete(ctrl.streams, id)
	delete(ctrl.metadataCollectors, id)

	return nil
}

var errStreamRemoved = errors.New("stream does not exist in state")

func (ctrl *Control) getStream(id types.ChannelID) (*Stream, error) {
	ctrl.stateMu.RLock()
	defer ctrl.stateMu.RUnlock()

	if _, exists := ctrl.streams[id]; !exists {
		return nil, errStreamRemoved
	}
	return ctrl.streams[id], nil
}

// activeStreamCount returns the number of streams currently in the local
// active-stream table. It is primarily intended for tests and shutdown logic.
func (ctrl *Control) activeStreamCount() int {
	ctrl.stateMu.RLock()
	defer ctrl.stateMu.RUnlock()

	return len(ctrl.streams)
}

// hasStream reports whether the channel is still present in the local
// active-stream table.
func (ctrl *Control) hasStream(id types.ChannelID) bool {
	ctrl.stateMu.RLock()
	defer ctrl.stateMu.RUnlock()

	_, exists := ctrl.streams[id]

	return exists
}
