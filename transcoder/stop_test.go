package transcoder

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xfrr/goffmpeg/media"
)

const fakeFFmpegModeEnv = "GOFFMPEG_FAKE_FFMPEG_MODE"

// TestMain lets the test binary act as a fake ffmpeg child process. That is
// how Run/Stop are exercised without requiring a real ffmpeg installation.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeFFmpegModeEnv); mode != "" {
		fakeFFmpegMain(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeFFmpegMain implements the child side of the tests.
//
// Modes:
//   - "progress:<frame>;<frame>;..." writes one ffmpeg progress line per frame
//   - "wait-quit" writes progress forever until it reads "q" or stdin EOF
//   - "stubborn" never reads stdin and runs forever (Stop must kill it)
//   - "output" writes a payload on stdout
//   - "drain-stdin" consumes stdin until EOF
func fakeFFmpegMain(mode string) {
	switch {
	case strings.HasPrefix(mode, "progress:"):
		frames := strings.Split(strings.TrimPrefix(mode, "progress:"), ";")
		for _, frame := range frames {
			writeProgressLine(frame)
			time.Sleep(5 * time.Millisecond)
		}
	case mode == "wait-quit":
		frame := 0
		for {
			frame++
			writeProgressLine(strconv.Itoa(frame))
			time.Sleep(5 * time.Millisecond)
			if mode != "" && stdinClosed() {
				return
			}
		}
	case mode == "stubborn":
		frame := 0
		for {
			frame++
			writeProgressLine(strconv.Itoa(frame))
			time.Sleep(5 * time.Millisecond)
		}
	case mode == "output":
		_, _ = os.Stdout.WriteString("PAYLOAD")
	case mode == "drain-stdin":
		_, _ = io.Copy(io.Discard, os.Stdin)
	}

	os.Exit(0)
}

var stdinPollOnce sync.Once
var stdinPollClosed chan struct{}

// stdinClosed reports whether the process' stdin reached EOF or carried "q".
func stdinClosed() bool {
	stdinPollOnce.Do(func() {
		stdinPollClosed = make(chan struct{})
		go func() {
			reader := bufio.NewReader(os.Stdin)
			for {
				b, err := reader.ReadByte()
				if err != nil {
					close(stdinPollClosed)
					return
				}
				if b == 'q' {
					close(stdinPollClosed)
					return
				}
			}
		}()
	})

	select {
	case <-stdinPollClosed:
		return true
	default:
		return false
	}
}

func writeProgressLine(frame string) {
	line := fmt.Sprintf("frame= %s fps=25 q=28.0 size= 256kB time=00:00:04.00 bitrate= 512.0kbits/s speed=1.5x\r", frame)
	_, _ = os.Stderr.WriteString(line)
}

// fakeProcess is the Run seam used by the tests: it starts this test binary in
// the requested fake-ffmpeg mode.
func fakeProcess(mode string) func([]string) *exec.Cmd {
	return func([]string) *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), fakeFFmpegModeEnv+"="+mode)
		return cmd
	}
}

// useFastTimings shortens the stop/teardown timeouts for the duration of a
// test so failure paths do not slow the suite down.
func useFastTimings(t *testing.T) {
	t.Helper()

	oldGrace, oldKill, oldWrite, oldDrain, oldProgress := stopGracePeriod, stopKillWait, stopWriteTimeout, outputDrainTimeout, progressDrainTimeout
	stopGracePeriod = 300 * time.Millisecond
	stopKillWait = time.Second
	stopWriteTimeout = 100 * time.Millisecond
	outputDrainTimeout = 300 * time.Millisecond
	progressDrainTimeout = 300 * time.Millisecond

	t.Cleanup(func() {
		stopGracePeriod, stopKillWait, stopWriteTimeout, outputDrainTimeout, progressDrainTimeout = oldGrace, oldKill, oldWrite, oldDrain, oldProgress
	})
}

func newFakeTranscoder(t *testing.T, mode string, duration string) *Transcoder {
	t.Helper()

	file := new(media.File)
	file.SetMetadata(media.Metadata{Format: media.Format{Duration: duration}})

	ts := new(Transcoder)
	ts.SetMediaFile(file)
	ts.newProcess = fakeProcess(mode)

	return ts
}

// drainProgress collects every update until the channel closes or the timeout
// expires. It reports whether the channel closed in time.
func drainProgress(t *testing.T, ch <-chan Progress, timeout time.Duration) ([]Progress, bool) {
	t.Helper()

	var got []Progress
	deadline := time.After(timeout)

	for {
		select {
		case p, ok := <-ch:
			if !ok {
				return got, true
			}
			got = append(got, p)
		case <-deadline:
			return got, false
		}
	}
}

func waitChannel(t *testing.T, ch <-chan error, timeout time.Duration) (error, bool) {
	t.Helper()

	select {
	case err := <-ch:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func TestParseProgressLine(t *testing.T) {
	t.Run("parses a regular ffmpeg progress line", func(t *testing.T) {
		ts := new(Transcoder)
		file := new(media.File)
		file.SetMetadata(media.Metadata{Format: media.Format{Duration: "10"}})
		ts.SetMediaFile(file)

		line := "frame= 100 fps=25 q=28.0 size= 256kB time=00:00:04.00 bitrate= 512.0kbits/s speed=1.5x"

		progress, ok := ts.parseProgressLine(line)
		require.True(t, ok)
		assert.Equal(t, "100", progress.FramesProcessed)
		assert.Equal(t, "00:00:04.00", progress.CurrentTime)
		assert.Equal(t, "512.0kbits/s", progress.CurrentBitrate)
		assert.Equal(t, "1.5x", progress.Speed)
		assert.InDelta(t, 40.0, progress.Progress, 0.0001)
	})

	t.Run("ignores lines that are not progress", func(t *testing.T) {
		ts := new(Transcoder)

		for _, line := range []string{
			"",
			"ffmpeg version 6.0 Copyright (c) 2000-2023",
			"frame= 100 time=00:00:04.00",
			"bitrate= 512.0kbits/s",
		} {
			_, ok := ts.parseProgressLine(line)
			assert.False(t, ok, "line %q should not be parsed as progress", line)
		}
	})

	t.Run("live streams do not divide by zero", func(t *testing.T) {
		ts := new(Transcoder)
		file := new(media.File)
		file.SetMetadata(media.Metadata{Format: media.Format{Duration: "0"}})
		ts.SetMediaFile(file)

		progress, ok := ts.parseProgressLine("frame= 10 fps=25 q=28.0 size= 1kB time=00:00:01.00 bitrate= 8.0kbits/s speed=1x")
		require.True(t, ok)
		assert.Equal(t, float64(0), progress.Progress)
		assert.False(t, math.IsNaN(progress.Progress))
		assert.False(t, math.IsInf(progress.Progress, 0))
	})

	t.Run("missing media metadata yields no percentage", func(t *testing.T) {
		ts := new(Transcoder)

		progress, ok := ts.parseProgressLine("frame= 10 fps=25 q=28.0 size= 1kB time=00:00:01.00 bitrate= 8.0kbits/s speed=1x")
		require.True(t, ok)
		assert.Equal(t, float64(0), progress.Progress)
	})
}

func TestRunReportsProgressUntilNaturalEnd(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2;3", "10")

	done := ts.Run(true)
	progress, closed := drainProgress(t, ts.Output(), 10*time.Second)

	assert.True(t, closed, "progress channel must close at the end of the run")
	require.NotEmpty(t, progress)
	assert.Equal(t, "3", progress[len(progress)-1].FramesProcessed)

	err, ok := waitChannel(t, done, 5*time.Second)
	require.True(t, ok)
	assert.NoError(t, err)
}

func TestStopClosesProgressChannel(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "wait-quit", "0")

	done := ts.Run(true)
	output := ts.Output()

	// Make sure the run is alive and reporting before stopping it.
	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- ts.Stop() }()

	select {
	case err := <-stopDone:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}

	// The consumer must observe the channel closing instead of blocking.
	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed, "progress channel must close after Stop")

	_, _ = waitChannel(t, done, 5*time.Second)
}

func TestStopKillsUncooperativeProcess(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "stubborn", "0")

	done := ts.Run(true)
	output := ts.Output()

	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	start := time.Now()
	err := ts.Stop()
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Less(t, elapsed, 5*time.Second, "Stop must not wait for a process that ignores the quit request")

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)

	_, ok := waitChannel(t, done, 5*time.Second)
	assert.True(t, ok, "the run must finish after Stop")
}

func TestStopAfterNaturalEndIsNoop(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2", "10")

	done := ts.Run(true)
	output := ts.Output()

	runErr, finished := waitChannel(t, done, 10*time.Second)
	require.True(t, finished)
	assert.NoError(t, runErr)

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)

	stopDone := make(chan error, 1)
	go func() { stopDone <- ts.Stop() }()

	select {
	case stopErr := <-stopDone:
		assert.NoError(t, stopErr)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked after the run had already finished")
	}
}

func TestStopWithoutRun(t *testing.T) {
	ts := new(Transcoder)

	stopDone := make(chan error, 1)
	go func() { stopDone <- ts.Stop() }()

	select {
	case err := <-stopDone:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked although no run had been started")
	}
}

func TestStopClosesInjectedStderrProgressChannel(t *testing.T) {
	useFastTimings(t)

	reader, writer := io.Pipe()
	ts := new(Transcoder)
	ts.SetProcessStderrPipe(reader)

	output := ts.Output()

	// A consumer is waiting on a stream that never reaches EOF by itself.
	go func() {
		_, _ = writer.Write([]byte("frame= 1 fps=25 q=28.0 size= 1kB time=00:00:01.00 bitrate= 8.0kbits/s speed=1x\r"))
	}()

	select {
	case <-output:
	case <-time.After(5 * time.Second):
		t.Fatal("no progress reported")
	}

	require.NoError(t, ts.Stop())

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed, "Stop must release a manually injected progress stream")
}

func TestRestartDoesNotMixProgressFromPreviousRun(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2;3", "10")

	done := ts.Run(true)
	first, closed := drainProgress(t, ts.Output(), 10*time.Second)
	require.True(t, closed)
	require.NotEmpty(t, first)
	_, _ = waitChannel(t, done, 5*time.Second)

	// A new round on the same instance must only report its own data.
	ts.newProcess = fakeProcess("progress:20;21")

	done = ts.Run(true)
	second, closed := drainProgress(t, ts.Output(), 10*time.Second)
	require.True(t, closed)
	require.NotEmpty(t, second)

	for _, p := range second {
		assert.NotContains(t, []string{"1", "2", "3"}, p.FramesProcessed,
			"progress from the previous run leaked into the new one")
	}
	assert.Equal(t, "21", second[len(second)-1].FramesProcessed)

	err, ok := waitChannel(t, done, 5*time.Second)
	require.True(t, ok)
	assert.NoError(t, err)
}

func TestStopDoesNotAffectNextRun(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "wait-quit", "0")

	done := ts.Run(true)
	output := ts.Output()

	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	require.NoError(t, ts.Stop())
	_, closed := drainProgress(t, output, 5*time.Second)
	require.True(t, closed)
	_, _ = waitChannel(t, done, 5*time.Second)

	ts.newProcess = fakeProcess("progress:7;8")
	done = ts.Run(true)

	progress, closed := drainProgress(t, ts.Output(), 10*time.Second)
	require.True(t, closed)
	require.NotEmpty(t, progress)
	assert.Equal(t, "8", progress[len(progress)-1].FramesProcessed)

	err, ok := waitChannel(t, done, 5*time.Second)
	require.True(t, ok)
	assert.NoError(t, err)
}

func TestStopConcurrentWithNaturalEnd(t *testing.T) {
	useFastTimings(t)

	for i := 0; i < 40; i++ {
		ts := newFakeTranscoder(t, "progress:1;2", "10")

		done := ts.Run(true)
		output := ts.Output()

		stopResult := make(chan error, 1)
		go func() { stopResult <- ts.Stop() }()

		_, closed := drainProgress(t, output, 5*time.Second)
		assert.True(t, closed, "iteration %d: progress channel must close exactly once", i)

		select {
		case err := <-stopResult:
			assert.NoError(t, err, "iteration %d", i)
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Stop did not return", i)
		}

		_, ok := waitChannel(t, done, 5*time.Second)
		assert.True(t, ok, "iteration %d: run must finish", i)
	}
}

func TestConcurrentStops(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "wait-quit", "0")

	done := ts.Run(true)
	output := ts.Output()

	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, ts.Stop())
		}()
	}
	wg.Wait()

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)

	_, _ = waitChannel(t, done, 5*time.Second)
}

func TestOutputChannelClosesOnlyOnce(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1", "10")

	done := ts.Run(true)
	output := ts.Output()

	// Reading after the end must yield the closed channel, never a panic from
	// a second close or a send on a closed channel.
	_, closed := drainProgress(t, output, 10*time.Second)
	assert.True(t, closed)
	_, closedAgain := drainProgress(t, output, time.Second)
	assert.True(t, closedAgain)

	require.NoError(t, ts.Stop())
	_, _ = waitChannel(t, done, 5*time.Second)
}

// trackingReadCloser records whether the transcoder closed the pipe it was
// handed.
type trackingReadCloser struct {
	io.ReadCloser
	mu     sync.Mutex
	closed bool
}

func (t *trackingReadCloser) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return t.ReadCloser.Close()
}

func (t *trackingReadCloser) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// trackingWriteCloser records whether the transcoder closed the pipe it was
// handed.
type trackingWriteCloser struct {
	io.WriteCloser
	mu     sync.Mutex
	closed bool
}

func (t *trackingWriteCloser) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return t.WriteCloser.Close()
}

func (t *trackingWriteCloser) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func TestInputPipeIsClosedOnNaturalEnd(t *testing.T) {
	useFastTimings(t)

	file := new(media.File)
	file.SetMetadata(media.Metadata{Format: media.Format{Duration: "10"}})
	inputReader, inputWriter := io.Pipe()
	tracked := &trackingReadCloser{ReadCloser: inputReader}
	file.SetInputPipe(true)
	file.SetInputPipeReader(tracked)
	file.SetInputPipeWriter(inputWriter)

	ts := new(Transcoder)
	ts.SetMediaFile(file)
	ts.newProcess = fakeProcess("drain-stdin")

	done := ts.Run(false)

	// The caller finishes feeding media and signals EOF by closing its writer.
	_, err := inputWriter.Write([]byte("some media bytes"))
	require.NoError(t, err)
	require.NoError(t, inputWriter.Close())

	runErr, ok := waitChannel(t, done, 10*time.Second)
	require.True(t, ok, "run must finish once the input reaches EOF")
	require.NoError(t, runErr)

	assert.Eventually(t, tracked.isClosed, 5*time.Second, 10*time.Millisecond,
		"the transcoder must close the input pipe reader when the run ends")
}

func TestInputPipeIsClosedOnStop(t *testing.T) {
	useFastTimings(t)

	file := new(media.File)
	file.SetMetadata(media.Metadata{Format: media.Format{Duration: "0"}})
	inputReader, inputWriter := io.Pipe()
	tracked := &trackingReadCloser{ReadCloser: inputReader}
	file.SetInputPipe(true)
	file.SetInputPipeReader(tracked)
	file.SetInputPipeWriter(inputWriter)

	ts := new(Transcoder)
	ts.SetMediaFile(file)
	ts.newProcess = fakeProcess("drain-stdin")

	done := ts.Run(false)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, ts.Stop())

	assert.True(t, tracked.isClosed(), "the transcoder must close the input pipe reader on Stop")

	_, ok := waitChannel(t, done, 5*time.Second)
	assert.True(t, ok)
}

func TestOutputPipeReceivesDataAndCloses(t *testing.T) {
	useFastTimings(t)

	file := new(media.File)
	file.SetMetadata(media.Metadata{Format: media.Format{Duration: "10"}})
	outputReader, outputWriter := io.Pipe()
	tracked := &trackingWriteCloser{WriteCloser: outputWriter}
	file.SetOutputPipe(true)
	file.SetOutputPipeReader(outputReader)
	file.SetOutputPipeWriter(tracked)

	ts := new(Transcoder)
	ts.SetMediaFile(file)
	ts.newProcess = fakeProcess("output")

	done := ts.Run(false)

	type readResult struct {
		data []byte
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(outputReader)
		readDone <- readResult{data: data, err: err}
	}()

	runErr, ok := waitChannel(t, done, 10*time.Second)
	require.True(t, ok, "run must finish")
	require.NoError(t, runErr)

	select {
	case res := <-readDone:
		assert.NoError(t, res.err)
		assert.Equal(t, "PAYLOAD", string(res.data))
	case <-time.After(5 * time.Second):
		t.Fatal("output pipe was not closed after the run ended")
	}

	assert.True(t, tracked.isClosed(), "the transcoder must close the output pipe writer")
}

func TestOutputPipeIsClosedOnStop(t *testing.T) {
	useFastTimings(t)

	file := new(media.File)
	file.SetMetadata(media.Metadata{Format: media.Format{Duration: "0"}})
	outputReader, outputWriter := io.Pipe()
	tracked := &trackingWriteCloser{WriteCloser: outputWriter}
	file.SetOutputPipe(true)
	file.SetOutputPipeReader(outputReader)
	file.SetOutputPipeWriter(tracked)

	ts := new(Transcoder)
	ts.SetMediaFile(file)
	ts.newProcess = fakeProcess("wait-quit")

	done := ts.Run(true)

	// Nobody reads the output pipe on purpose: Stop must not depend on a
	// consumer draining it.
	output := ts.Output()
	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	stopResult := make(chan error, 1)
	go func() { stopResult <- ts.Stop() }()

	select {
	case err := <-stopResult:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop blocked while the output pipe was unread")
	}

	// Now consume whatever was buffered; the pipe must end.
	_, err := io.ReadAll(outputReader)
	assert.NoError(t, err)

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)

	_, ok := waitChannel(t, done, 5*time.Second)
	assert.True(t, ok)

	assert.True(t, tracked.isClosed(), "the transcoder must close the output pipe writer on Stop")
}

func TestRunWithoutMediaFileFails(t *testing.T) {
	ts := new(Transcoder)

	err, ok := waitChannel(t, ts.Run(true), time.Second)
	require.True(t, ok)
	assert.Error(t, err)
}

func TestOutputWithoutProgressStreamYieldsEmptyUpdate(t *testing.T) {
	ts := new(Transcoder)

	progress, closed := drainProgress(t, ts.Output(), 2*time.Second)
	assert.True(t, closed)
	assert.Len(t, progress, 1)
	assert.Equal(t, Progress{}, progress[0])
}

func TestUnconsumedProgressChannelDoesNotBlockRun(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2;3;4;5", "10")

	// Progress is requested but Output is never called: the run must still
	// finish instead of blocking on an unconsumed channel.
	done := ts.Run(true)

	err, ok := waitChannel(t, done, 10*time.Second)
	require.True(t, ok, "the run must not depend on progress being consumed")
	assert.NoError(t, err)

	// And the channel, once requested, is already closed.
	progress, closed := drainProgress(t, ts.Output(), 5*time.Second)
	assert.True(t, closed)
	assert.LessOrEqual(t, len(progress), 5)
}

func TestProgressIsNotDroppedWhileConsumed(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2;3;4;5", "10")

	done := ts.Run(true)
	output := ts.Output()

	var frames []string
	for p := range output {
		frames = append(frames, p.FramesProcessed)
	}

	err, ok := waitChannel(t, done, 5*time.Second)
	require.True(t, ok)
	assert.NoError(t, err)
	assert.Equal(t, []string{"1", "2", "3", "4", "5"}, frames,
		"a consumed progress stream must not lose updates")
}

func TestManualProgressStreamIsRetiredByNewRun(t *testing.T) {
	useFastTimings(t)

	reader, writer := io.Pipe()
	ts := new(Transcoder)
	ts.SetProcessStderrPipe(reader)

	manual := ts.Output()

	go func() {
		_, _ = writer.Write([]byte("frame= 1 fps=25 q=28.0 size= 1kB time=00:00:01.00 bitrate= 8.0kbits/s speed=1x\r"))
	}()

	select {
	case <-manual:
	case <-time.After(5 * time.Second):
		t.Fatal("no progress on the manual stream")
	}

	// Starting a real run must retire the manual stream, otherwise a reader
	// of it would hang forever.
	ts.SetMediaFile(&media.File{})
	ts.MediaFile().SetMetadata(media.Metadata{Format: media.Format{Duration: "10"}})
	ts.newProcess = fakeProcess("progress:1;2")

	done := ts.Run(true)

	_, closed := drainProgress(t, manual, 5*time.Second)
	assert.True(t, closed, "the manual progress stream must close when a new run starts")

	runProgress, closed := drainProgress(t, ts.Output(), 10*time.Second)
	assert.True(t, closed)
	require.NotEmpty(t, runProgress)
	assert.Equal(t, "2", runProgress[len(runProgress)-1].FramesProcessed)

	_, ok := waitChannel(t, done, 5*time.Second)
	assert.True(t, ok)
}

// blockingWriteCloser blocks forever, emulating an unresponsive stdin writer.
type blockingWriteCloser struct{}

func (blockingWriteCloser) Write(p []byte) (int, error) {
	time.Sleep(time.Hour)
	return len(p), nil
}

func (blockingWriteCloser) Close() error { return nil }

func TestStopWithBlockingInjectedStdin(t *testing.T) {
	useFastTimings(t)

	ts := new(Transcoder)
	ts.SetProcessStdinPipe(blockingWriteCloser{})

	stopDone := make(chan error, 1)
	go func() { stopDone <- ts.Stop() }()

	select {
	case err := <-stopDone:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on an unresponsive stdin writer")
	}
}

func TestOutputIsIdempotentPerRun(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1;2", "10")
	done := ts.Run(true)

	first := ts.Output()
	second := ts.Output()
	assert.Equal(t, first, second, "Output must return the same channel for one run")

	_, closed := drainProgress(t, first, 10*time.Second)
	assert.True(t, closed)
	_, _ = waitChannel(t, done, 5*time.Second)
}

func TestStopWithUnreadProgress(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "wait-quit", "0")
	done := ts.Run(true)

	// Take the channel but never read from it: Stop must not depend on a
	// consumer making progress.
	output := ts.Output()
	_ = output

	time.Sleep(100 * time.Millisecond)

	stopDone := make(chan error, 1)
	go func() { stopDone <- ts.Stop() }()

	select {
	case err := <-stopDone:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked while the progress channel was unread")
	}

	_, _ = waitChannel(t, done, 5*time.Second)

	// Once the consumer does read, it must see a closed channel.
	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)
}

func TestStopImmediatelyAfterRun(t *testing.T) {
	useFastTimings(t)

	for i := 0; i < 20; i++ {
		ts := newFakeTranscoder(t, "wait-quit", "0")

		done := ts.Run(true)

		stopDone := make(chan error, 1)
		go func() { stopDone <- ts.Stop() }()

		select {
		case err := <-stopDone:
			require.NoError(t, err, "iteration %d", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: Stop blocked", i)
		}

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: run never finished", i)
		}
	}
}

func TestOutputAfterRunWithoutProgress(t *testing.T) {
	useFastTimings(t)

	ts := newFakeTranscoder(t, "progress:1", "10")

	done := ts.Run(false)
	_, _ = waitChannel(t, done, 10*time.Second)

	// Progress was not requested for this run: Output keeps the historical
	// behaviour of a single empty update on an already closed channel.
	progress, closed := drainProgress(t, ts.Output(), 2*time.Second)
	assert.True(t, closed)
	assert.Len(t, progress, 1)
	assert.Equal(t, Progress{}, progress[0])
}

func TestStopWithNoRunClosesManualStream(t *testing.T) {
	useFastTimings(t)

	reader, writer := io.Pipe()
	ts := new(Transcoder)
	ts.SetProcessStderrPipe(reader)

	output := ts.Output()

	go func() {
		_, _ = writer.Write([]byte("frame= 1 fps=25 q=28.0 size= 1kB time=00:00:01.00 bitrate= 8.0kbits/s speed=1x\r"))
	}()

	select {
	case <-output:
	case <-time.After(5 * time.Second):
		t.Fatal("no progress reported")
	}

	require.NoError(t, ts.Stop())

	_, closed := drainProgress(t, output, 5*time.Second)
	assert.True(t, closed)
}
