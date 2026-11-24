package rtspclient

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
)

// fakeRTSPConfig tunes the in-process fake RTSP server.
type fakeRTSPConfig struct {
	// describeStatus makes OnDescribe reply with this status code instead of
	// serving the stream (zero means 200 OK).
	describeStatus base.StatusCode

	// blockDescribe / blockPlay stall the corresponding handler until the
	// channel is closed, simulating a hung camera. The test always closes the
	// channel in cleanup, so a server shutdown never waits on a blocked handler.
	blockDescribe chan struct{}
	blockPlay     chan struct{}

	// noFrames, when true, accepts PLAY but never sends a frame.
	noFrames bool
}

// fakeRTSPServer is an offline, loopback-only RTSP server backed by
// gortsplib's own Server, so tests need no real camera.
type fakeRTSPServer struct {
	t       *testing.T
	cfg     fakeRTSPConfig
	address string

	s       *gortsplib.Server
	stream  *gortsplib.ServerStream
	media   *description.Media
	h264    *format.H264
	encoder *rtph264.Encoder

	mu            sync.Mutex
	sessionCount  int
	sessionClosed int
	serverDone    chan struct{}

	// sendFrames gates RTP pumping at runtime; flipping it lets one server
	// simulate a stalled camera and then recover without a second listener.
	sendFrames bool

	// dialedConns are the client-side connections created through
	// Client.DialContext, one per grab. gortsplib owns their data I/O; tests
	// only observe their close notification to assert teardown.
	connMu      sync.Mutex
	dialedConns []*clientSignalConn
}

func newFakeRTSPServer(t *testing.T, cfg fakeRTSPConfig) *fakeRTSPServer {
	t.Helper()

	srv := &fakeRTSPServer{
		t:          t,
		cfg:        cfg,
		serverDone: make(chan struct{}),
		sendFrames: !cfg.noFrames,
	}
	// Unblock any handler still waiting when the test ends. This runs before
	// the server's own Close cleanup (cleanups run LIFO, and the server Close
	// is registered below), so the handler has returned by shutdown time.
	if cfg.blockDescribe != nil {
		t.Cleanup(func() {
			select {
			case <-cfg.blockDescribe:
			default:
				close(cfg.blockDescribe)
			}
		})
	}
	if cfg.blockPlay != nil {
		t.Cleanup(func() {
			select {
			case <-cfg.blockPlay:
			default:
				close(cfg.blockPlay)
			}
		})
	}

	// Minimal but structurally valid H264 parameter sets and an IDR slice.
	// The bytes never reach a real decoder in tests (ffmpeg is stubbed); they
	// only need to be classified correctly by gortsplib's RTP depacketizer.
	srv.h264 = &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:               []byte{0x68, 0xce, 0x38, 0x80},
	}
	srv.media = &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{srv.h264},
	}

	enc, err := srv.h264.CreateEncoder()
	if err != nil {
		t.Fatalf("create H264 encoder: %v", err)
	}
	srv.encoder = enc

	srv.s = &gortsplib.Server{
		Handler:      srv,
		RTSPAddress:  "127.0.0.1:0",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	if err := srv.s.Start(); err != nil {
		t.Fatalf("start fake RTSP server: %v", err)
	}
	t.Cleanup(func() {
		srv.s.Close()
		close(srv.serverDone)
	})

	srv.address = srv.s.NetListener().Addr().String()

	// Route the gortsplib client's dialer through a hook that wraps every
	// dialed connection, so tests can assert the client closes it on teardown.
	prevHook := dialContextHook
	dialContextHook = func(_ context.Context, network, address string) (net.Conn, error) {
		c, err := net.Dial(network, address)
		if err != nil {
			return nil, err
		}
		sc := newClientSignalConn(c)
		srv.connMu.Lock()
		srv.dialedConns = append(srv.dialedConns, sc)
		srv.connMu.Unlock()
		return sc, nil
	}
	t.Cleanup(func() { dialContextHook = prevHook })

	srv.stream = &gortsplib.ServerStream{
		Server: srv.s,
		Desc:   &description.Session{Medias: []*description.Media{srv.media}},
	}
	if err := srv.stream.Initialize(); err != nil {
		t.Fatalf("initialize server stream: %v", err)
	}
	t.Cleanup(srv.stream.Close)

	return srv
}

// urlWithCredentials builds a loopback rtsp URL for the fake server, carrying
// distinctive credentials so tests can assert they never leak into errors.
func (s *fakeRTSPServer) urlWithCredentials() string {
	return fmt.Sprintf("rtsp://camuser:campassword@%s/stream1", s.address)
}

func (s *fakeRTSPServer) urlWithoutCredentials() string {
	return fmt.Sprintf("rtsp://%s/stream1", s.address)
}

func (s *fakeRTSPServer) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
	s.mu.Lock()
	s.sessionCount++
	s.mu.Unlock()
}

func (s *fakeRTSPServer) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	s.sessionClosed++
	s.mu.Unlock()
}

// waitWhileBlocked stalls until release is closed or the test server shuts
// down. It never touches the socket, so it cannot interfere with gortsplib's
// own connection reader; cleanup always closes release first.
func (s *fakeRTSPServer) waitWhileBlocked(release chan struct{}) {
	if release == nil {
		return
	}
	select {
	case <-release:
	case <-s.serverDone:
	}
}

func (s *fakeRTSPServer) OnDescribe(
	_ *gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	s.waitWhileBlocked(s.cfg.blockDescribe)
	if s.cfg.describeStatus != 0 {
		return &base.Response{StatusCode: s.cfg.describeStatus}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *fakeRTSPServer) OnSetup(
	_ *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *fakeRTSPServer) OnPlay(
	_ *gortsplib.ServerHandlerOnPlayCtx,
) (*base.Response, error) {
	s.waitWhileBlocked(s.cfg.blockPlay)
	// Every PLAY starts its own pump so sequential client sessions each
	// receive frames; writes start failing once the client disconnects.
	go s.pumpFrames()
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// setSendFrames toggles frame delivery at runtime, letting a test stall a
// camera and then make it healthy again against the same listener.
func (s *fakeRTSPServer) setSendFrames(v bool) {
	s.mu.Lock()
	s.sendFrames = v
	s.mu.Unlock()
}

// pumpFrames emits one access unit (SPS + PPS + IDR) repeatedly so clients
// waiting for a random-access frame obtain one quickly.
func (s *fakeRTSPServer) pumpFrames() {
	au := [][]byte{
		bytesClone(s.h264.SPS),
		bytesClone(s.h264.PPS),
		{0x65, 0xb8, 0x00, 0x04, 0x00, 0x00, 0x00, 0x01},
	}
	packets, err := s.encoder.Encode(au)
	if err != nil {
		s.t.Errorf("fake server encode: %v", err)
		return
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.serverDone:
			return
		case <-ticker.C:
			s.mu.Lock()
			send := s.sendFrames
			s.mu.Unlock()
			if !send {
				continue
			}
			for _, pkt := range packets {
				if err := s.stream.WritePacketRTP(s.media, pkt); err != nil {
					return
				}
			}
		}
	}
}

// waitForTornDown polls until the most recent client connection opened during
// the test has been closed. This is the lifecycle guarantee the task cares
// about: a timed-out or canceled grab must not keep its RTSP connection
// alive. Earlier connections belong to prior grabs against other servers and
// are intentionally excluded.
func (s *fakeRTSPServer) waitForTornDown(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.latestConnClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	so, sc := s.sessionCount, s.sessionClosed
	s.mu.Unlock()
	t.Fatalf("RTSP client connection not torn down (sessions opened=%d closed=%d)", so, sc)
}

// latestConnClosed reports whether at least one connection was dialed and the
// most recent one has been closed by its client.
func (s *fakeRTSPServer) latestConnClosed() bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if len(s.dialedConns) == 0 {
		return false
	}
	latest := s.dialedConns[len(s.dialedConns)-1]
	select {
	case <-latest.closed:
		return true
	default:
		return false
	}
}

// clientSignalConn is a client-side net.Conn wrapper whose closed channel
// fires when gortsplib calls Close. All I/O is delegated; the wrapper only
// adds test observability and is installed through Client.DialContext.
type clientSignalConn struct {
	net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

func newClientSignalConn(c net.Conn) *clientSignalConn {
	return &clientSignalConn{Conn: c, closed: make(chan struct{})}
}

func (c *clientSignalConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}

// --- fake ffmpeg -----------------------------------------------------------
//
// The test binary re-executes itself as the "ffmpeg" process, keeping the
// whole suite offline and working on both Windows and POSIX systems.

const fakeFFmpegEnv = "CAMESNAP_TEST_FAKE_FFMPEG"

// installFakeFFmpeg points ffmpegBinary at the running test executable.
func installFakeFFmpeg(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(fakeFFmpegEnv, mode)
	prev := ffmpegBinary
	ffmpegBinary = os.Args[0]
	t.Cleanup(func() { ffmpegBinary = prev })
}

// maybeRunFakeFFmpeg is invoked from TestMain. When the re-exec marker is
// present it runs the stub behavior and never returns.
func maybeRunFakeFFmpeg() {
	mode := os.Getenv(fakeFFmpegEnv)
	if mode == "" {
		return
	}

	args := os.Args[1:]
	switch mode {
	case "success", "slow-success":
		if mode == "slow-success" {
			time.Sleep(2 * time.Second)
		}
		// The last argument is the output path, mirroring how GrabFrameViaGort
		// places outPath at the end of the ffmpeg command line.
		out := args[len(args)-1]
		if err := os.WriteFile(out, []byte("fake-jpeg"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)

	case "fail":
		fmt.Fprintf(os.Stderr,
			"ffmpeg failed to open rtsp://camuser:campassword@127.0.0.1:554/stream1: 401 Unauthorized\n")
		os.Exit(1)

	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "unknown fake ffmpeg mode %q\n", mode)
		os.Exit(2)
	}
}

func bytesClone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
