package control

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Glimesh/waveguide/pkg/keyframer"
	"github.com/Glimesh/waveguide/pkg/types"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

type StreamTrack struct {
	Type  webrtc.RTPCodecType
	Codec string
	Track webrtc.TrackLocal
}

type Stream struct {
	log logrus.FieldLogger

	cancelFunc context.CancelFunc
	// stopped is set atomically once Stop has torn down the local stream
	// (cancelled its context). It makes Stop safe to call any number of times
	// from any goroutine.
	stopped int32

	// idMu guards streamID, which is assigned during StartStream and read by
	// the independently-scheduled ingest and teardown goroutines.
	idMu     sync.RWMutex
	streamID types.StreamID

	// authenticated is set after the stream has successfully authed with a remote service
	authenticated bool

	whepURI string

	saveVideo   bool
	videoWriter FileWriter

	// mediaStarted is set after media bytes have come in from the client
	mediaStarted bool
	hasSomeAudio bool
	hasSomeVideo bool

	kf            *keyframer.Keyframer
	rtpIngest     chan *rtp.Packet
	lastThumbnail chan []byte
	// channel used to signal thumbnailer to stop
	stopThumbnailer   chan struct{}
	requestThumbnail  chan struct{}
	thumbnailReceiver chan *rtp.Packet

	videoWriterChan chan *rtp.Packet

	ChannelID types.ChannelID
	StreamKey types.StreamKey

	tracks []StreamTrack

	// Raw Metadata
	startTime           int64
	lastTime            int64 // Last time the metadata collector ran
	audioBps            int
	videoBps            int
	totalAudioPackets   int
	totalVideoPackets   int
	lastAudioPackets    int
	lastVideoPackets    int
	clientVendorName    string
	clientVendorVersion string
	videoCodec          string
	audioCodec          string
	videoHeight         int
	videoWidth          int
}

func (s *Stream) AddTrack(track webrtc.TrackLocal, codec string) error {
	// TODO: Needs better support for tracks with different codecs
	if track.Kind() == webrtc.RTPCodecTypeAudio {
		s.hasSomeAudio = true
		s.audioCodec = codec
	} else if track.Kind() == webrtc.RTPCodecTypeVideo {
		s.hasSomeVideo = true
		s.videoCodec = codec
	} else {
		return errors.New("unexpected track kind")
	}

	s.tracks = append(s.tracks, StreamTrack{
		Type:  track.Kind(),
		Track: track,
		Codec: codec,
	})

	return nil
}

func (s *Stream) ReportMetadata(metadatas ...Metadata) error {
	for _, metadata := range metadatas {
		metadata(s)
	}

	return nil
}

// Stop cancels the stream's ingest context. It is idempotent and safe to
// call concurrently from any number of goroutines: only the first call
// performs the actual cancellation.
func (s *Stream) Stop() {
	if !atomic.CompareAndSwapInt32(&s.stopped, 0, 1) {
		// Already stopped; do nothing.
		return
	}

	s.log.Infof("stopping stream")

	s.cancelFunc()
	s.log.Debug("canceled stream ctx")
}

func (s *Stream) Stopped() bool {
	return atomic.LoadInt32(&s.stopped) == 1
}

// SetStreamID records the stream ID returned by the external service. It is
// safe to call before the ingest/teardown goroutines start reading it.
func (s *Stream) SetStreamID(id types.StreamID) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	s.streamID = id
}

// StreamID returns the externally-assigned stream ID in a goroutine-safe way.
func (s *Stream) StreamID() types.StreamID {
	s.idMu.RLock()
	defer s.idMu.RUnlock()

	return s.streamID
}
