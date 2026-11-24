package test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xfrr/goffmpeg"
	"github.com/xfrr/goffmpeg/transcoder"
)

const (
	fixturePath = "./fixtures"
	resultsPath = "./test_results"
)

var (
	// Input files
	input3gp = path.Join(fixturePath, "input.3gp")
)

// requireFFmpeg skips the test when ffmpeg/ffprobe are not installed. The
// package itself is a wrapper: without the binaries there is nothing to
// exercise.
func requireFFmpeg(t *testing.T) {
	t.Helper()

	if _, err := goffmpeg.Configure(context.Background()); err != nil {
		t.Skipf("ffmpeg/ffprobe not available: %v", err)
	}
}

func TestInputNotFound(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)
	var outputPath = path.Join(resultsPath, "notfound.mp4")

	trans := new(transcoder.Transcoder)

	err := trans.Initialize("notfound.3gp", outputPath)
	assert.NotNil(t, err)
}

func TestTranscodingProgress(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "progress.mp4")
	trans := new(transcoder.Transcoder)

	err := trans.Initialize(input3gp, outputPath)
	assert.Nil(t, err)

	errCh := trans.Run(true)

	progress := []transcoder.Progress{}
	for val := range trans.Output() {
		progress = append(progress, val)
	}
	err = <-errCh
	assert.Nil(t, err)
	assert.GreaterOrEqual(t, len(progress), 1)
	checkFileExists(t, outputPath)
}

// TestTranscodingProgressParsedValues checks the values that the parser is
// expected to expose for a regular, finite input.
func TestTranscodingProgressParsedValues(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "progress-values.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.Initialize(input3gp, outputPath))

	errCh := trans.Run(true)

	var progress []transcoder.Progress
	for val := range trans.Output() {
		progress = append(progress, val)
	}
	require.NoError(t, <-errCh)
	require.NotEmpty(t, progress)

	last := progress[len(progress)-1]
	assert.NotEmpty(t, last.FramesProcessed)
	assert.NotEmpty(t, last.CurrentTime)
	assert.NotEmpty(t, last.CurrentBitrate)
	assert.Greater(t, last.Progress, 0.0)
	assert.LessOrEqual(t, last.Progress, 100.0)
}

// TestStopClosesProgressChannel is the end-to-end version of the reported
// problem: a consumer reads progress, Stop is called mid-transcode, and the
// consumer must observe the channel being closed instead of blocking forever.
func TestStopClosesProgressChannel(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "stopped.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.Initialize(input3gp, outputPath))

	// Slow the encode down so there is a real window to stop in.
	trans.MediaFile().SetPreset("slow")
	trans.MediaFile().SetNativeFramerateInput(true)

	errCh := trans.Run(true)
	output := trans.Output()

	// Wait until progress is flowing, then stop.
	select {
	case _, ok := <-output:
		require.True(t, ok, "progress channel closed before progress was reported")
	case <-time.After(30 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- trans.Stop() }()

	select {
	case err := <-stopErr:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return")
	}

	// The channel must close; ranging over it must not hang.
	drained := make(chan struct{})
	go func() {
		for range output { //nolint:revive
		}
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("progress channel did not close after Stop")
	}

	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not finish after Stop")
	}

	// Stopping an already stopped transcoder must be harmless.
	assert.NoError(t, trans.Stop())
}

// TestStopWhenFFmpegIgnoresQuit covers the case where ffmpeg cannot be asked
// to quit through stdin (for instance with -nostdin, or when stdin carries
// media). Stop must still terminate the run in bounded time and the progress
// channel must close instead of leaving the consumer blocked for the rest of
// the transcode.
func TestStopWhenFFmpegIgnoresQuit(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "nostdin.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.Initialize(input3gp, outputPath))
	trans.MediaFile().SetPreset("veryslow")
	trans.MediaFile().SetNativeFramerateInput(true)
	trans.MediaFile().SetRawInputArgs([]string{"-nostdin"})

	errCh := trans.Run(true)
	output := trans.Output()

	select {
	case <-output:
	case <-time.After(30 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- trans.Stop() }()

	select {
	case err := <-stopErr:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Stop blocked although ffmpeg ignored the quit request")
	}

	drained := make(chan struct{})
	go func() {
		for range output { //nolint:revive
		}
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("progress channel did not close after Stop")
	}

	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not finish after Stop")
	}
}

// TestStopAfterNaturalEnd makes sure Stop is a no-op once the transcode ended
// on its own.
func TestStopAfterNaturalEnd(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "stopped-after-end.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.Initialize(input3gp, outputPath))

	errCh := trans.Run(true)
	for range trans.Output() { //nolint:revive
	}
	require.NoError(t, <-errCh)

	stopErr := make(chan error, 1)
	go func() { stopErr <- trans.Stop() }()

	select {
	case err := <-stopErr:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop blocked after the transcode had already finished")
	}
}

// TestRestartAfterStop verifies that a stopped transcoder can be started again
// and that the new round reports its own progress.
func TestRestartAfterStop(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "restart.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.Initialize(input3gp, outputPath))
	trans.MediaFile().SetPreset("slow")
	trans.MediaFile().SetNativeFramerateInput(true)

	errCh := trans.Run(true)
	optionalRead := trans.Output()

	select {
	case <-optionalRead:
	case <-time.After(30 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	require.NoError(t, trans.Stop())
	for range optionalRead { //nolint:revive
	}
	<-errCh

	// Restart the same instance with a fast preset and no artificial delay.
	outputPath2 := path.Join(resultsPath, "restart-2.mp4")
	require.NoError(t, trans.SetOutputPath(outputPath2))
	trans.MediaFile().SetPreset("ultrafast")
	trans.MediaFile().SetNativeFramerateInput(false)

	errCh = trans.Run(true)

	var second []transcoder.Progress
	for p := range trans.Output() {
		second = append(second, p)
	}

	runErr := <-errCh
	require.NoError(t, runErr)
	require.NotEmpty(t, second, "the second run must report its own progress")
	assert.Greater(t, second[len(second)-1].Progress, 0.0)
	checkFileExists(t, outputPath2)
}

// TestInputPipeIsClosedAfterRun checks that the transcoder releases the input
// pipe it was given, so no pipe is leaked.
func TestInputPipeIsClosedAfterRun(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "pipe-input.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.InitializeEmptyTranscoder())
	trans.MediaFile().SetOutputPath(outputPath)
	trans.MediaFile().SetVideoCodec("libx264")
	trans.MediaFile().SetPreset("ultrafast")
	trans.MediaFile().SetVframes(3)

	inputWriter, err := trans.CreateInputPipe()
	require.NoError(t, err)

	errCh := trans.Run(false)

	// Feed the fixture through the pipe and close it to signal EOF.
	data, err := os.ReadFile(input3gp)
	require.NoError(t, err)
	_, err = inputWriter.Write(data)
	require.NoError(t, err)
	require.NoError(t, inputWriter.Close())

	select {
	case <-errCh:
	case <-time.After(60 * time.Second):
		t.Fatal("run did not finish after the input pipe reached EOF")
	}

	// A second write must fail: the reader side was closed by the transcoder.
	assert.Eventually(t, func() bool {
		_, err := inputWriter.Write([]byte("x"))
		return err != nil
	}, 10*time.Second, 50*time.Millisecond, "input pipe must be released after the run")
}

// TestStopWithInputPipe covers a run that is fed through an input pipe: Stop
// must end the run even though ffmpeg's stdin carries media rather than its
// quit command, and the caller's pipe must be released.
func TestStopWithInputPipe(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	outputPath := path.Join(resultsPath, "pipe-stopped.mp4")
	trans := new(transcoder.Transcoder)

	require.NoError(t, trans.InitializeEmptyTranscoder())
	trans.MediaFile().SetOutputPath(outputPath)
	trans.MediaFile().SetVideoCodec("libx264")
	trans.MediaFile().SetPreset("ultrafast")

	inputWriter, err := trans.CreateInputPipe()
	require.NoError(t, err)

	errCh := trans.Run(true)
	output := trans.Output()

	data, err := os.ReadFile(input3gp)
	require.NoError(t, err)

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		for i := 0; i < len(data); i += 4096 {
			end := i + 4096
			if end > len(data) {
				end = len(data)
			}
			if _, err := inputWriter.Write(data[i:end]); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	select {
	case <-output:
	case <-time.After(30 * time.Second):
		t.Fatal("no progress reported before Stop")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- trans.Stop() }()

	select {
	case err := <-stopErr:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Stop blocked for a run fed through an input pipe")
	}

	drained := make(chan struct{})
	go func() {
		for range output { //nolint:revive
		}
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("progress channel did not close after Stop")
	}

	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not finish after Stop")
	}

	select {
	case <-feedDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the input pipe writer stayed blocked: the pipe was leaked")
	}
}

// TestOutputPipeIsClosedAfterRun checks that the output pipe delivers data and
// is closed when the transcode ends.
func TestOutputPipeIsClosedAfterRun(t *testing.T) {
	requireFFmpeg(t)
	createResultsDir(t)

	trans := new(transcoder.Transcoder)
	require.NoError(t, trans.Initialize(input3gp, ""))
	trans.MediaFile().SetOutputPath("")
	trans.MediaFile().SetVideoCodec("libx264")
	trans.MediaFile().SetPreset("ultrafast")

	outputReader, err := trans.CreateOutputPipe("mp4")
	require.NoError(t, err)

	errCh := trans.Run(false)

	readDone := make(chan error, 1)
	var total int
	go func() {
		for {
			buf := make([]byte, 32*1024)
			n, err := outputReader.Read(buf)
			total += n
			if err == io.EOF {
				readDone <- nil
				return
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()

	select {
	case <-errCh:
	case <-time.After(60 * time.Second):
		t.Fatal("run did not finish")
	}

	select {
	case err := <-readDone:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("output pipe was not closed when the transcode ended")
	}

	assert.Greater(t, total, 0, "the output pipe must have delivered data")
}

func createResultsDir(t *testing.T) {
	t.Helper()

	err := exec.Command("mkdir", "-p", resultsPath).Run()
	assert.Nil(t, err)
}

func checkFileExists(t *testing.T, filepath string) {
	t.Helper()

	res, err := exec.Command("cat", filepath).Output()
	assert.Nil(t, err)
	assert.Greater(t, len(res), 0)
}
