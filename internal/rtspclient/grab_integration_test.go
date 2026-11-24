package rtspclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

const (
	secretUser = "camuser"
	secretPass = "campassword"
)

// assertRedacted fails if an error message leaks the fixture credentials.
// When the message embeds an RTSP URL it must also carry the REDACTED
// marker and keep the host/path for diagnosis.
func assertRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	for _, secret := range []string{secretUser, secretPass, secretUser + ":" + secretPass} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaks credential %q: %s", secret, msg)
		}
	}
	if strings.Contains(msg, "rtsp://") || strings.Contains(msg, "rtsps://") {
		if !strings.Contains(msg, "REDACTED") {
			t.Fatalf("RTSP URL in error was not redacted: %s", msg)
		}
	}
}

// assertRedactedURL is like assertRedacted but requires the message to embed
// a redacted RTSP URL (used for stages whose failures quote the stream URL).
func assertRedactedURL(t *testing.T, err error) {
	t.Helper()
	assertRedacted(t, err)
	if !strings.Contains(err.Error(), "REDACTED@") {
		t.Fatalf("error does not show redacted userinfo: %s", err.Error())
	}
}

// assertNoTempFiles ensures a failed grab leaves no half-written transcode
// temp file in the output directory.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".camsnap-grab-") {
			t.Fatalf("temp transcode file was not cleaned up: %s", e.Name())
		}
	}
}

func TestGrabParseErrorRedactsCredentials(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jpg")
	// A bad percent-escape makes url.Parse fail with an error that quotes the
	// offending input, which carries credentials that must not leak.
	err := GrabFrameViaGort(context.Background(),
		"rtsp://camuser:campassword@127.0.0.1:554/stream1/%zz",
		"tcp", out, time.Second)
	if err == nil || !strings.Contains(err.Error(), "parse url") {
		t.Fatalf("expected a parse error, got %v", err)
	}
	assertRedactedURL(t, err)
	if !strings.Contains(err.Error(), "127.0.0.1:554/stream1") {
		t.Fatalf("parse error lost location info: %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("no output file must exist after parse failure, stat err = %v", statErr)
	}
}

func TestGrabDescribeErrorRedactsCredentials(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{describeStatus: base.StatusNotFound})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	err := GrabFrameViaGort(context.Background(), srv.urlWithCredentials(),
		"tcp", out, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected a 404 describe error, got %v", err)
	}
	// The status-code error carries no URL at all, but the stream URL held
	// credentials: the failure must still be safe to paste into a ticket.
	assertRedacted(t, err)
	if strings.Contains(err.Error(), secretUser) {
		t.Fatalf("describe error leaked credentials: %v", err)
	}
	srv.waitForTornDown(t)
}

func TestGrabDescribeTimeoutReturnsSentinelAndTearsDown(t *testing.T) {
	release := make(chan struct{})
	srv := newFakeRTSPServer(t, fakeRTSPConfig{blockDescribe: release})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	start := time.Now()
	err := GrabFrameViaGort(context.Background(), srv.urlWithCredentials(),
		"tcp", out, 300*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrGrabTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ErrGrabTimeout wrapping DeadlineExceeded, got %v", err)
	}
	assertRedacted(t, err)
	if elapsed > 3*time.Second {
		t.Fatalf("grab did not return promptly on timeout: %s", elapsed)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("output file must not exist after timed-out describe")
	}
	srv.waitForTornDown(t)

	// Releasing the blocked handler now must not panic or affect later grabs:
	// the connection is already gone, the handler exits via connDone.
	close(release)
}

func TestGrabPlayTimeoutReturnsSentinelAndTearsDown(t *testing.T) {
	release := make(chan struct{})
	srv := newFakeRTSPServer(t, fakeRTSPConfig{blockPlay: release})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	err := GrabFrameViaGort(context.Background(), srv.urlWithCredentials(),
		"tcp", out, 300*time.Millisecond)
	if !errors.Is(err, ErrGrabTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ErrGrabTimeout, got %v", err)
	}
	assertRedacted(t, err)
	srv.waitForTornDown(t)
	close(release)
}

func TestGrabNoFramesTimeoutTearsDownSession(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{noFrames: true})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	start := time.Now()
	err := GrabFrameViaGort(context.Background(), srv.urlWithCredentials(),
		"tcp", out, 300*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrGrabTimeout) {
		t.Fatalf("expected ErrGrabTimeout, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("frame wait was not bounded by timeout: %s", elapsed)
	}
	srv.waitForTornDown(t)
}

func TestGrabParentCancelationReturnsCanceled(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{noFrames: true})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	err := GrabFrameViaGort(ctx, srv.urlWithCredentials(), "tcp", out, 10*time.Second)
	if !errors.Is(err, ErrGrabCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ErrGrabCanceled wrapping context.Canceled, got %v", err)
	}
	assertRedacted(t, err)
	srv.waitForTornDown(t)
}

func TestGrabSuccessWritesOutput(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{})
	installFakeFFmpeg(t, "success")
	out := filepath.Join(t.TempDir(), "out.jpg")

	if err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", out, 5*time.Second); err != nil {
		t.Fatalf("grab failed: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "fake-jpeg" {
		t.Fatalf("output = %q, want fake-jpeg", string(data))
	}
	srv.waitForTornDown(t)
}

func TestGrabFFmpegFailureRedactsAndRemovesOutput(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{})
	installFakeFFmpeg(t, "fail")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jpg")

	err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", out, 5*time.Second)
	if err == nil {
		t.Fatal("expected ffmpeg failure")
	}
	// The stub prints a credential-bearing URL on stderr; it must be redacted
	// before the error leaves the capture path.
	assertRedactedURL(t, err)
	if !strings.Contains(err.Error(), "127.0.0.1:554/stream1") {
		t.Fatalf("ffmpeg error lost location info: %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("half-written output must be removed after ffmpeg failure")
	}
	assertNoTempFiles(t, dir)
	srv.waitForTornDown(t)
}

// TestGrabFFmpegFailurePreservesPreExistingOutput makes sure a failed
// transcode can never clobber an output file the caller already owns.
func TestGrabFFmpegFailurePreservesPreExistingOutput(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{})
	installFakeFFmpeg(t, "fail")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jpg")
	original := []byte("caller-owned-content")
	if err := os.WriteFile(out, original, 0o644); err != nil {
		t.Fatal(err)
	}

	err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", out, 5*time.Second)
	if err == nil {
		t.Fatal("expected ffmpeg failure")
	}
	got, statErr := os.ReadFile(out)
	if statErr != nil {
		t.Fatalf("pre-existing output disappeared after failed grab: %v", statErr)
	}
	if string(got) != string(original) {
		t.Fatalf("pre-existing output was modified: got %q want %q", got, original)
	}
	assertNoTempFiles(t, dir)
}

func TestGrabFFmpegTimeoutKillsProcessAndRemovesOutput(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{})
	installFakeFFmpeg(t, "hang")
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jpg")

	start := time.Now()
	err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", out, 400*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrGrabTimeout) {
		t.Fatalf("expected ErrGrabTimeout from ffmpeg stage, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hung ffmpeg was not killed on timeout: %s", elapsed)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("output must not remain after ffmpeg timeout")
	}
	assertNoTempFiles(t, dir)
	srv.waitForTornDown(t)
}

// TestSequentialGrabsDoNotInterfere proves a timed-out grab against a stalled
// camera leaves no state that corrupts a subsequent grab once the same camera
// becomes healthy again. One fake server stands in for one physical camera.
func TestSequentialGrabsDoNotInterfere(t *testing.T) {
	srv := newFakeRTSPServer(t, fakeRTSPConfig{noFrames: true})
	installFakeFFmpeg(t, "success")
	dir := t.TempDir()

	// First grab: the camera is not sending frames, so the grab times out and
	// must tear its connection down without leaving an output file.
	badOut := filepath.Join(dir, "bad.jpg")
	err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", badOut, 250*time.Millisecond)
	if !errors.Is(err, ErrGrabTimeout) {
		t.Fatalf("first grab should time out, got %v", err)
	}
	srv.waitForTornDown(t)
	if _, statErr := os.Stat(badOut); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("first grab must not leave a partial output behind")
	}

	// The camera recovers and starts streaming. A second grab must succeed and
	// write its own output, independent of the first call's timeout.
	srv.setSendFrames(true)
	goodOut := filepath.Join(dir, "good.jpg")
	if err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", goodOut, 5*time.Second); err != nil {
		t.Fatalf("second grab failed after first grab timed out: %v", err)
	}
	if _, statErr := os.Stat(goodOut); statErr != nil {
		t.Fatalf("second grab produced no output: %v", statErr)
	}
	srv.waitForTornDown(t)

	// A third grab also succeeds, showing no client or connection is held open
	// between calls.
	thirdOut := filepath.Join(dir, "third.jpg")
	if err := GrabFrameViaGort(context.Background(), srv.urlWithoutCredentials(),
		"tcp", thirdOut, 5*time.Second); err != nil {
		t.Fatalf("third grab failed: %v", err)
	}
	if _, statErr := os.Stat(thirdOut); statErr != nil {
		t.Fatalf("third grab produced no output: %v", statErr)
	}
	srv.waitForTornDown(t)
}

func TestGrabInvalidTransport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jpg")
	err := GrabFrameViaGort(context.Background(), "rtsp://127.0.0.1:554/s",
		"sctp", out, time.Second)
	if err == nil || !strings.Contains(err.Error(), "invalid transport") {
		t.Fatalf("expected invalid transport error, got %v", err)
	}
}
