// Package rtspclient provides RTSP helpers using gortsplib.
package rtspclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
	"github.com/steipete/camsnap/internal/redact"
)

// ErrGrabTimeout is returned when no frame (or no completed transcode) is
// produced before the caller timeout. It wraps context.DeadlineExceeded, so
// errors.Is(err, context.DeadlineExceeded) keeps working.
var ErrGrabTimeout = errors.New("rtsp frame grab timed out")

// ErrGrabCanceled is returned when the caller canceled the grab before a
// frame was produced. It wraps context.Canceled.
var ErrGrabCanceled = errors.New("rtsp frame grab canceled")

// ffmpegBinary is the transcode executable. It is a variable so tests can
// point it at a local stub without touching PATH.
var ffmpegBinary = "ffmpeg"

// dialContextHook, when non-nil, replaces gortsplib's default dialer. It is
// intended solely for offline tests that need to observe connection teardown
// by wrapping the client-owned socket; production code leaves it nil.
var dialContextHook func(ctx context.Context, network, address string) (net.Conn, error)

// GrabFrameViaGort connects with gortsplib, reads until a random-access (IDR)
// frame, then pipes it to ffmpeg to save a JPEG.
//
// Guarantees:
//   - Every error message is safe to share: userinfo in any RTSP/RTSPS URL
//     (from parse, describe, setup, play or ffmpeg output) is replaced by
//     REDACTED while host, port and path are kept.
//   - The whole session, including the gortsplib client, its reader goroutine
//     and the ffmpeg process, is bounded by ctx/timeout: on timeout or
//     cancelation everything is torn down and a half-written output file is
//     removed.
//   - Calls are independent: each grab owns its client, process and output
//     path, so a failed or timed-out grab cannot affect the next one.
func GrabFrameViaGort(ctx context.Context, rawURL, transport, outPath string, timeout time.Duration) error {
	if transport == "" {
		transport = "udp"
	}

	grabCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u, err := base.ParseURL(rawURL)
	if err != nil {
		// net/url parse errors quote the offending input, which may contain
		// credentials; redact rather than returning the raw parse error.
		return redact.Errorf("parse url: %v", err)
	}

	switch transport {
	case "udp", "tcp":
	default:
		return fmt.Errorf("invalid transport %q", transport)
	}

	cl := &gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	if dialContextHook != nil {
		cl.DialContext = dialContextHook
	}
	p := gortsplib.ProtocolUDP
	if transport == "tcp" {
		p = gortsplib.ProtocolTCP
	}
	cl.Protocol = &p

	if err := cl.Start(); err != nil {
		return redact.Errorf("start: %v", err)
	}

	// closeOnce makes teardown idempotent: the context watcher and the normal
	// return path can both ask for the client to shut down. Close blocks until
	// the gortsplib run goroutine and transport routines have exited, so no
	// goroutine or socket outlives this call.
	var closeOnce sync.Once
	closeClient := func() { closeOnce.Do(cl.Close) }
	defer closeClient()

	// gortsplib's Client is driven by its own internal context and does not
	// react to the caller's context. Close() cancels that internal context and
	// unblocks any in-flight Describe/Setup/Play/Wait, which is how the grab
	// stays strictly aligned with the caller deadline.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-grabCtx.Done():
			closeClient()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	desc, _, err := cl.Describe(u)
	if err != nil {
		return stageError(grabCtx, "describe", err)
	}

	medi, fmtH264 := findH264(desc.Medias)
	if medi == nil || fmtH264 == nil {
		return fmt.Errorf("no H264 track found")
	}

	// ensure auth propagated to setup/play
	if u.User != nil && desc.BaseURL != nil {
		desc.BaseURL.User = u.User
	}

	if _, err := cl.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return stageError(grabCtx, "setup video", err)
	}

	dec, err := fmtH264.CreateDecoder()
	if err != nil {
		return redact.Errorf("decoder: %v", err)
	}

	sample := h264Sample{sps: bytes.Clone(fmtH264.SPS), pps: bytes.Clone(fmtH264.PPS)}
	var frame []byte
	var sampleMu sync.Mutex
	done := make(chan struct{})
	errCh := make(chan error, 1)

	cl.OnPacketRTP(medi, fmtH264, func(pkt *rtp.Packet) {
		select {
		case <-done:
			return
		default:
		}
		nalus, err := dec.Decode(pkt)
		if err != nil {
			return
		}
		if len(nalus) == 0 {
			return
		}
		sampleMu.Lock()
		defer sampleMu.Unlock()
		select {
		case <-done:
			return
		default:
		}
		if frame = sample.frame(nalus); frame != nil {
			close(done)
		}
	})

	if _, err := cl.Play(nil); err != nil {
		return stageError(grabCtx, "play", err)
	}

	go func() {
		if err := cl.Wait(); err != nil {
			errCh <- err
		}
	}()

	var sampleData []byte
	select {
	case <-done:
		sampleMu.Lock()
		sampleData = frame
		sampleMu.Unlock()
	case err := <-errCh:
		return stageError(grabCtx, "rtsp client", err)
	case <-grabCtx.Done():
		return contextError(grabCtx)
	}

	if err := transcodeFrame(grabCtx, sampleData, outPath); err != nil {
		return err
	}
	return nil
}

// stageError wraps a gortsplib stage failure. When the grab context has
// expired, the stage error is usually the library's "terminated" signal
// caused by our own Close(): report the caller-recognizable timeout/cancel
// error instead. All other failures are returned with redacted URLs.
func stageError(grabCtx context.Context, stage string, err error) error {
	if ctxErr := grabCtx.Err(); ctxErr != nil {
		return contextError(grabCtx)
	}
	return redact.Errorf("%s: %v", stage, err)
}

// contextError maps the expired grab context to the package sentinels while
// preserving the standard library errors.Is chain.
func contextError(grabCtx context.Context) error {
	if errors.Is(grabCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrGrabTimeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("%w: %w", ErrGrabCanceled, context.Canceled)
}

// transcodeFrame feeds the collected H264 sample to ffmpeg to produce the
// JPEG. ffmpeg writes a sibling temp file which is only renamed onto outPath
// after a fully successful run, so a failure, timeout or cancelation never
// leaves a half-written output behind. CommandContext kills and reaps the
// process when ctx expires, so no transcoder can outlive the grab.
func transcodeFrame(ctx context.Context, sampleData []byte, outPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".camsnap-grab-*.jpg.tmp")
	if err != nil {
		return fmt.Errorf("create temp output: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("prepare temp output: %w", err)
	}

	cmd := exec.CommandContext(ctx, ffmpegBinary,
		"-y",
		"-f", "h264",
		"-i", "pipe:0",
		"-frames:v", "1",
		tmpPath,
	)
	cmd.Stdin = bytes.NewReader(sampleData)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		_ = os.Remove(tmpPath)
		// CommandContext kills the process when ctx expires; CombinedOutput
		// only returns once the process has been reaped. Drop the executable's
		// own error text in that case and report the recognizable sentinel.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return contextError(ctx)
		}
		return redact.Errorf("ffmpeg write frame: %v\n%s", runErr, string(out))
	}

	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize output: %w", err)
	}
	return nil
}

type h264Sample struct {
	sps, pps []byte
}

func (s *h264Sample) frame(nalus [][]byte) []byte {
	keyframe := false
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			s.sps = bytes.Clone(nalu)
		case h264.NALUTypePPS:
			s.pps = bytes.Clone(nalu)
		case h264.NALUTypeIDR:
			keyframe = true
		}
	}
	if !keyframe {
		return nil
	}

	// Cameras may send codec parameters only in SDP, or update them in-band.
	// Feed ffmpeg those parameters and the IDR, without undecodable earlier frames.
	var sample bytes.Buffer
	for _, nalu := range append([][]byte{s.sps, s.pps}, nalus...) {
		if len(nalu) != 0 {
			sample.Write([]byte{0, 0, 0, 1})
			sample.Write(nalu)
		}
	}
	return sample.Bytes()
}

func findH264(medias []*description.Media) (*description.Media, *format.H264) {
	for _, m := range medias {
		for _, f := range m.Formats {
			if h, ok := f.(*format.H264); ok {
				return m, h
			}
		}
	}
	return nil, nil
}
