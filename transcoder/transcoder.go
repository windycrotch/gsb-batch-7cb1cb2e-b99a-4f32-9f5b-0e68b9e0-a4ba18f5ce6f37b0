package transcoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xfrr/goffmpeg"
	"github.com/xfrr/goffmpeg/media"
	"github.com/xfrr/goffmpeg/pkg/duration"
)

const (
	// quitCommand is what ffmpeg reads from its control stdin to finish
	// gracefully.
	quitCommand = "q\n"
	// progressBufferSize is the number of parsed updates buffered so a
	// momentarily busy consumer does not stall the reader (and with it the
	// child process' stderr pipe).
	progressBufferSize = 16
)

// The durations below are variables so tests can shorten them.
var (
	// stopGracePeriod is how long Stop waits for ffmpeg to quit gracefully
	// after being asked to, before the process is killed.
	stopGracePeriod = 2 * time.Second
	// stopKillWait bounds how long Stop waits for a killed process to be
	// reaped before it releases the progress stream anyway.
	stopKillWait = 2 * time.Second
	// stopWriteTimeout bounds how long Stop may block writing to ffmpeg's
	// stdin.
	stopWriteTimeout = 250 * time.Millisecond
	// outputDrainTimeout bounds how long the end of a run waits for the
	// output pump to forward the data ffmpeg already produced.
	outputDrainTimeout = 1 * time.Second
	// progressDrainTimeout bounds how long the end of a run waits for the
	// progress reader to deliver the last updates to a slow consumer.
	progressDrainTimeout = 1 * time.Second
)

// Transcoder Main struct
type Transcoder struct {
	mu                 sync.Mutex
	stdErrPipe         io.ReadCloser
	stdStdinPipe       io.WriteCloser
	process            *exec.Cmd
	mediafile          *media.File
	configuration      goffmpeg.Configuration
	whiteListProtocols []string

	// currentRun holds the state of the most recent (or still running) run.
	// A new run replaces it, so nothing from an old run can leak into a new
	// one.
	currentRun *transcoderRun

	// manualPipe is the progress stream used when Output is called without a
	// run (for instance after SetProcessStderrPipe). manualStop is its stop
	// signal, manualReaderDone is closed once its reader closed the channel
	// and manualOutput is the channel returned to callers; all are created
	// once so repeated Output calls share them.
	manualPipe       io.ReadCloser
	manualOutput     chan Progress
	manualStop       chan struct{}
	manualReaderDone chan struct{}
	manualOnce       sync.Once

	// newProcess builds the ffmpeg command. It is a test seam; production
	// code leaves it nil and exec.Command is used.
	newProcess func(command []string) *exec.Cmd
}

// transcoderRun groups everything that belongs to one ffmpeg invocation.
type transcoderRun struct {
	process *exec.Cmd

	// stderr is the parent's read end of the progress stream and stdin the
	// parent's write end of the child's stdin pipe.
	stderr io.ReadCloser
	stdin  io.WriteCloser
	// controlStdin reports whether stdin is ffmpeg's control terminal (as
	// opposed to a pipe carrying media data).
	controlStdin bool

	// childStderr, childStdin and childStdout are the transcoder's copies of
	// the ends handed to the child. They are closed right after Start so the
	// child becomes the only owner of the other side: that is what makes the
	// progress stream reach EOF when the process exits.
	childStderr *os.File
	childStdin  *os.File
	childStdout *os.File

	// inputPipe and outputPipe are the media pipes owned by the caller. They
	// are always closed when the run ends, however it ends.
	inputPipe  io.ReadCloser
	outputPipe io.WriteCloser

	// stop is closed when Stop is requested. It lets the progress reader stop
	// producing updates for a consumer that may no longer be reading.
	stop chan struct{}
	// started is closed once the OS process was created (or failed to start).
	started chan struct{}
	// done is closed once the ffmpeg process has been reaped and its pipes
	// have been released.
	done chan struct{}

	// stopSingle serialises Stop: only one caller performs the shutdown, the
	// others wait and observe the same result.
	stopSingle sync.Once
	stopDone   chan struct{}
	stopErr    error
	// output is the progress channel of this run. It is created and owned by
	// the run and closed exactly once by the progress reader.
	output chan Progress
	// readerDone is closed by the progress reader once its channel is closed.
	readerDone chan struct{}
	// outTaken reports whether Output handed the channel to a consumer. When
	// nobody consumes it there is no point in waiting for updates to be
	// delivered before closing the channel.
	outTaken atomic.Bool

	stopOnce      sync.Once
	startedOnce   sync.Once
	doneOnce      sync.Once
	stdinOnce     sync.Once
	stderrOnce    sync.Once
	inputOnce     sync.Once
	outputOnce    sync.Once
	childEndsOnce sync.Once
}

func newTranscoderRun(proc *exec.Cmd) *transcoderRun {
	return &transcoderRun{
		process:    proc,
		stop:       make(chan struct{}),
		started:    make(chan struct{}),
		done:       make(chan struct{}),
		stopDone:   make(chan struct{}),
		readerDone: make(chan struct{}),
	}
}

func (r *transcoderRun) requestStop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *transcoderRun) markStarted() {
	r.startedOnce.Do(func() { close(r.started) })
}

func (r *transcoderRun) markDone() {
	r.doneOnce.Do(func() { close(r.done) })
}

// waitReader blocks until the progress reader has closed the output channel.
// When no reader was started it returns immediately.
func (r *transcoderRun) waitReader(timeout time.Duration) {
	if r.output == nil {
		return
	}
	select {
	case <-r.readerDone:
	case <-time.After(timeout):
	}
}

// releaseProgress closes the progress stream and waits for the reader to
// close the output channel. Stop relies on this: once it returns, a consumer
// ranging over Output must see the channel closed.
func (r *transcoderRun) releaseProgress() {
	r.closeStderr()
	r.waitReader(stopKillWait)
}

// closeChildEnds drops the transcoder's copies of the ends handed to the
// child process. Without this, the progress reader never reaches EOF (the
// write end would stay open in the parent) and ffmpeg never sees stdin EOF.
func (r *transcoderRun) closeChildEnds() {
	r.childEndsOnce.Do(func() {
		if r.childStderr != nil {
			_ = r.childStderr.Close()
			r.childStderr = nil
		}
		if r.childStdin != nil {
			_ = r.childStdin.Close()
			r.childStdin = nil
		}
		if r.childStdout != nil {
			_ = r.childStdout.Close()
			r.childStdout = nil
		}
	})
}

func (r *transcoderRun) closeStdin() {
	if r.stdin != nil {
		r.stdinOnce.Do(func() { _ = r.stdin.Close() })
	}
}

func (r *transcoderRun) closeStderr() {
	if r.stderr != nil {
		r.stderrOnce.Do(func() { _ = r.stderr.Close() })
	}
}

func (r *transcoderRun) closeInput() {
	if r.inputPipe != nil {
		r.inputOnce.Do(func() { _ = r.inputPipe.Close() })
	}
}

func (r *transcoderRun) closeOutput() {
	if r.outputPipe != nil {
		r.outputOnce.Do(func() { _ = r.outputPipe.Close() })
	}
}

// SetProcessStderrPipe Set the STDERR pipe
func (t *Transcoder) SetProcessStderrPipe(v io.ReadCloser) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stdErrPipe = v
}

// SetProcessStdinPipe Set the STDIN pipe
func (t *Transcoder) SetProcessStdinPipe(v io.WriteCloser) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stdStdinPipe = v
}

// SetProcess Set the transcoding process
func (t *Transcoder) SetProcess(cmd *exec.Cmd) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.process = cmd
}

// SetMediaFile Set the media file
func (t *Transcoder) SetMediaFile(v *media.File) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mediafile = v
}

// SetConfiguration Set the transcoding configuration
func (t *Transcoder) SetConfiguration(v goffmpeg.Configuration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.configuration = v
}

func (t *Transcoder) SetWhiteListProtocols(availableProtocols []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.whiteListProtocols = availableProtocols
}

// Process Get transcoding process
func (t *Transcoder) Process() *exec.Cmd {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.process
}

// MediaFile Get the ttranscoding media file.
func (t *Transcoder) MediaFile() *media.File {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mediafile
}

// FFmpegExec Get FFmpeg Bin path
func (t *Transcoder) FFmpegExec() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.configuration.FFmpegBinPath()
}

// FFprobeExec Get FFprobe Bin path
func (t *Transcoder) FFprobeExec() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.configuration.FFprobeBinPath()
}

// GetCommand Build and get command
func (t *Transcoder) GetCommand() []string {
	t.mu.Lock()
	mediaFile := t.mediafile
	whiteList := t.whiteListProtocols
	t.mu.Unlock()

	if mediaFile == nil {
		return nil
	}

	rcommand := append([]string{"-y"}, mediaFile.ToStrCommand()...)

	if whiteList != nil {
		rcommand = append([]string{"-protocol_whitelist", strings.Join(whiteList, ",")}, rcommand...)
	}

	return rcommand
}

// InitializeEmptyTranscoder initializes the fields necessary for a blank transcoder
func (t *Transcoder) InitializeEmptyTranscoder() error {
	var Metadata media.Metadata

	var err error
	cfg := t.configuration
	if len(cfg.FFmpegBinPath()) == 0 || len(cfg.FFprobeBinPath()) == 0 {
		cfg, err = goffmpeg.Configure(context.Background())
		if err != nil {
			return err
		}
	}
	// Set new File
	MediaFile := new(media.File)
	MediaFile.SetMetadata(Metadata)

	// Set transcoder configuration
	t.SetMediaFile(MediaFile)
	t.SetConfiguration(cfg)
	return nil
}

// SetInputPath sets the input path for transcoding
func (t *Transcoder) SetInputPath(inputPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mediafile == nil {
		return errors.New("media file not initialized")
	}
	if t.mediafile.InputPipe() {
		return errors.New("cannot set an input path when an input pipe command has been set")
	}
	t.mediafile.SetInputPath(inputPath)
	return nil
}

// SetOutputPath sets the output path for transcoding
func (t *Transcoder) SetOutputPath(inputPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mediafile == nil {
		return errors.New("media file not initialized")
	}
	if t.mediafile.OutputPipe() {
		return errors.New("cannot set an input path when an input pipe command has been set")
	}
	t.mediafile.SetOutputPath(inputPath)
	return nil
}

// CreateInputPipe creates an input pipe for the transcoding process
func (t *Transcoder) CreateInputPipe() (*io.PipeWriter, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mediafile == nil {
		return nil, errors.New("media file not initialized")
	}
	if t.mediafile.InputPath() != "" {
		return nil, errors.New("cannot set an input pipe when an input path exists")
	}
	inputPipeReader, inputPipeWriter := io.Pipe()
	t.mediafile.SetInputPipe(true)
	t.mediafile.SetInputPipeReader(inputPipeReader)
	t.mediafile.SetInputPipeWriter(inputPipeWriter)
	return inputPipeWriter, nil
}

// CreateOutputPipe creates an output pipe for the transcoding process
func (t *Transcoder) CreateOutputPipe(containerFormat string) (*io.PipeReader, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mediafile == nil {
		return nil, errors.New("media file not initialized")
	}
	if t.mediafile.OutputPath() != "" {
		return nil, errors.New("cannot set an output pipe when an output path exists")
	}
	t.mediafile.SetOutputFormat(containerFormat)

	t.mediafile.SetMovFlags("frag_keyframe")
	outputPipeReader, outputPipeWriter := io.Pipe()
	t.mediafile.SetOutputPipe(true)
	t.mediafile.SetOutputPipeReader(outputPipeReader)
	t.mediafile.SetOutputPipeWriter(outputPipeWriter)
	return outputPipeReader, nil
}

// Initialize Init the transcoding process
func (t *Transcoder) Initialize(inputPath string, outputPath string) error {
	var err error
	var outb, errb bytes.Buffer
	var Metadata media.Metadata

	t.mu.Lock()
	cfg := t.configuration
	whiteList := t.whiteListProtocols
	t.mu.Unlock()

	if len(cfg.FFmpegBinPath()) == 0 || len(cfg.FFprobeBinPath()) == 0 {
		cfg, err = goffmpeg.Configure(context.Background())
		if err != nil {
			return err
		}
	}

	if inputPath == "" {
		return errors.New("error on transcoder.Initialize: inputPath missing")
	}

	command := []string{"-i", inputPath, "-print_format", "json", "-show_format", "-show_streams", "-show_error"}

	if whiteList != nil {
		command = append([]string{"-protocol_whitelist", strings.Join(whiteList, ",")}, command...)
	}

	cmd := exec.Command(cfg.FFprobeBinPath(), command...)
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("error executing (%s) | error: %s | message: %s %s", command, err, outb.String(), errb.String())
	}

	if err = json.Unmarshal(outb.Bytes(), &Metadata); err != nil {
		return err
	}

	// Set new File
	MediaFile := new(media.File)
	MediaFile.SetMetadata(Metadata)
	MediaFile.SetInputPath(inputPath)
	MediaFile.SetOutputPath(outputPath)

	// Set transcoder configuration
	t.SetMediaFile(MediaFile)
	t.SetConfiguration(cfg)

	return nil

}

// newProcessFor builds the exec.Cmd used for a run. The seam exists so tests
// can drive Run/Stop without an ffmpeg binary.
func (t *Transcoder) newProcessFor(command []string) *exec.Cmd {
	t.mu.Lock()
	builder := t.newProcess
	binPath := t.configuration.FFmpegBinPath()
	t.mu.Unlock()

	if builder != nil {
		return builder(command)
	}
	return exec.Command(binPath, command...)
}

// Run Starts the transcoding process
func (t *Transcoder) Run(progress bool) <-chan error {
	done := make(chan error, 1)

	t.mu.Lock()
	mediaFile := t.mediafile
	t.mu.Unlock()

	if mediaFile == nil {
		done <- errors.New("failed start ffmpeg: transcoder has no media file, call Initialize first")
		close(done)
		return done
	}

	command := t.GetCommand()
	if !progress {
		command = append([]string{"-nostats", "-loglevel", "0"}, command...)
	}

	proc := t.newProcessFor(command)
	run := newTranscoderRun(proc)

	if mediaFile.InputPipe() {
		run.inputPipe = mediaFile.InputPipeReader()
	}
	if mediaFile.OutputPipe() {
		run.outputPipe = mediaFile.OutputPipeWriter()
	}

	// The transcoder owns every pipe handed to the child. exec.Cmd would
	// otherwise spawn copying goroutines that cannot be unblocked without
	// closing the caller's pipes, and would close the progress stream as soon
	// as the process exits, possibly truncating buffered updates.
	if progress {
		readSide, writeSide, err := os.Pipe()
		if err != nil {
			fmt.Println("Progress not available: " + err.Error())
		} else {
			proc.Stderr = writeSide
			run.stderr = readSide
			run.childStderr = writeSide
		}
	}

	// ffmpeg's stdin is its control terminal, except when media arrives
	// through an input pipe.
	{
		readSide, writeSide, err := os.Pipe()
		if err != nil {
			fmt.Println("Stdin not available: " + err.Error())
		} else {
			proc.Stdin = readSide
			run.stdin = writeSide
			run.childStdin = readSide
			run.controlStdin = !mediaFile.InputPipe()
		}
	}

	// If the user has requested progress, ffmpeg's stdout is collected for
	// error messages. An output pipe takes precedence below.
	var outb, errb bytes.Buffer
	if progress {
		proc.Stdout = &outb
	}

	// If an output pipe has been set, ffmpeg writes into a transcoder owned
	// pipe that is pumped into the caller's pipe. This keeps the caller's
	// pipe closeable at any time and never leaves a copy behind.
	var outputPumpSource *os.File
	if mediaFile.OutputPipe() {
		readSide, writeSide, err := os.Pipe()
		if err != nil {
			fmt.Println("Output pipe not available: " + err.Error())
			run.closeOutput()
		} else {
			proc.Stdout = writeSide
			run.childStdout = writeSide
			outputPumpSource = readSide
		}
	}

	// With the pipes above, Wait only has the stdout buffer copy left; the
	// delay is a safety net against a stalled copier.
	proc.WaitDelay = outputDrainTimeout

	// The progress reader is started with the run, not when the caller asks
	// for Output: it must drain the stream as it is produced, and it must be
	// the single goroutine that closes the channel whether the run ends
	// naturally, is stopped, or both happen at the same time.
	if run.stderr != nil {
		run.output = make(chan Progress, progressBufferSize)
		go t.readProgress(run.stderr, run.stop, run.output, nil, func() {
			run.closeStderr()
			close(run.readerDone)
		})
	}
	// Publish this run before starting it, so Stop can find it as soon as Run
	// returns. A previous run is retired: its progress stream must not outlive
	// it and mix data into the new round.
	t.mu.Lock()
	previous := t.currentRun
	t.process = proc
	if run.stdin != nil {
		t.stdStdinPipe = run.stdin
	}
	if run.stderr != nil {
		t.stdErrPipe = run.stderr
	}
	// A manual (Output-without-Run) progress stream, if any, belongs to the
	// past now: stop it so it cannot outlive this run or be reused.
	manualStop := t.manualStop
	manualReaderDone := t.manualReaderDone
	manualPipe := t.manualPipe
	t.manualStop = nil
	t.manualReaderDone = nil
	t.manualPipe = nil
	t.manualOutput = nil
	// Allow a later Output call (once this run is over) to start a fresh
	// manual reader instead of returning the retired channel.
	t.manualOnce = sync.Once{}
	t.currentRun = run
	t.mu.Unlock()

	if previous != nil {
		previous.requestStop()
		previous.closeStderr()
	}

	// A manual progress stream from before this run is retired: leaving it
	// open would keep a consumer of it waiting forever.
	stopManualProgress(manualStop, manualPipe, manualReaderDone)

	if err := proc.Start(); err != nil {
		run.closeChildEnds()
		run.markStarted()
		run.markDone()
		run.closeStdin()
		run.closeStderr()
		run.closeInput()
		run.closeOutput()

		done <- fmt.Errorf("failed start ffmpeg (%s) with %s, message %s %s", command, err, outb.String(), errb.String())
		close(done)
		return done
	}

	run.markStarted()

	// The child owns its copies now; the parent must drop its own or the
	// progress reader would never see EOF and ffmpeg would never see stdin
	// EOF.
	run.closeChildEnds()

	// Pump the media input into ffmpeg's stdin.
	if run.inputPipe != nil && run.stdin != nil {
		go func(src io.Reader) {
			_, _ = io.Copy(run.stdin, src)
			run.closeStdin()
		}(run.inputPipe)
	}

	// Pump ffmpeg's stdout into the caller's output pipe, then close it. The
	// pump signals completion so the end of the run waits for the data, but
	// still closes the pipe when an abandoned consumer leaves it blocked.
	outputPumpDone := make(chan struct{})
	if outputPumpSource != nil && run.outputPipe != nil {
		go func(src *os.File, dst io.WriteCloser) {
			defer close(outputPumpDone)
			defer src.Close()
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
		}(outputPumpSource, run.outputPipe)
	} else {
		if outputPumpSource != nil {
			_ = outputPumpSource.Close()
		}
		close(outputPumpDone)
	}

	// Capture the teardown timeouts now: the completion goroutine outlives Run
	// and must not read package state that tests may be restoring.
	pumpDrainTimeout := outputDrainTimeout
	drainTimeout := progressDrainTimeout
	killTimeout := stopKillWait

	go func() {
		err := proc.Wait()
		if err != nil {
			err = fmt.Errorf("failed finish ffmpeg (%s) with %s message %s %s", command, err, outb.String(), errb.String())
		}

		// The process is gone: release the media pipes. The progress stream is
		// released by its reader, which reaches EOF once the child's write end
		// is the only one left and closes.
		run.closeInput()
		run.closeStdin()

		select {
		case <-outputPumpDone:
		case <-time.After(pumpDrainTimeout):
		}
		run.closeOutput()

		run.markDone()

		done <- err
		close(done)

		// Drain the progress stream after the completion signal, so Run is not
		// delayed by a consumer that reads slowly. A consumer that went away
		// must not keep the channel open forever, so it is eventually released.
		run.drainProgress(drainTimeout, killTimeout)
	}()

	return done
}

// drainProgress waits for the progress reader to finish. A consumer that is
// no longer reading would block it on a send, so after a grace period the
// reader is told to stop and the channel is closed anyway.
func (r *transcoderRun) drainProgress(drainTimeout, killTimeout time.Duration) {
	if r.output == nil {
		return
	}

	if !r.outTaken.Load() {
		// Nobody ever consumed the channel: there is nothing worth waiting for.
		r.requestStop()
	}

	select {
	case <-r.readerDone:
		return
	case <-time.After(drainTimeout):
	}

	r.requestStop()
	select {
	case <-r.readerDone:
	case <-time.After(killTimeout):
	}
}

// Stop Ends the transcoding process.
//
// Stop can be called at any time and from any goroutine:
//   - when no process is running (never started, already finished or already
//     stopped) it returns nil immediately without blocking;
//   - otherwise it asks ffmpeg to quit, gives it a short grace period to
//     finish, and kills it if it does not comply.
//
// As soon as Stop returns, a consumer ranging over the channel from Output
// observes the channel being closed, because the progress stream is released
// before Stop returns. The media input and output pipes are released as well.
func (t *Transcoder) Stop() error {
	t.mu.Lock()
	run := t.currentRun
	stdin := t.stdStdinPipe
	stdErrPipe := t.stdErrPipe
	mediaFile := t.mediafile
	manualStop := t.manualStop
	manualReaderDone := t.manualReaderDone
	manualPipe := t.manualPipe
	t.manualStop = nil
	t.manualReaderDone = nil
	t.manualPipe = nil
	t.mu.Unlock()

	if run == nil {
		// Nothing was started through Run (or the pipes were injected
		// manually). Release the progress stream so a reader cannot block
		// forever, and close the media pipes if they exist.
		stopManualProgress(manualStop, manualPipe, manualReaderDone)
		if stdin != nil {
			_ = writeWithTimeout(stdin, []byte(quitCommand))
			_ = stdin.Close()
		}
		if stdErrPipe != nil {
			_ = stdErrPipe.Close()
		}
		closeMediaPipes(mediaFile)
		return nil
	}

	// A leftover manual progress stream (created before this run) must not
	// keep its consumer waiting either.
	stopManualProgress(manualStop, manualPipe, manualReaderDone)

	return run.stopRun()
}

// stopRun performs (or waits for) the shutdown of a single run. Concurrent Stop
// calls all observe the same outcome.
func (r *transcoderRun) stopRun() error {
	r.stopSingle.Do(func() {
		defer close(r.stopDone)
		r.stopErr = r.shutdown()
	})

	<-r.stopDone
	return r.stopErr
}

func (r *transcoderRun) shutdown() error {
	// Make the progress reader stop producing updates for a consumer that may
	// no longer be reading.
	r.requestStop()

	// Closing the caller's input pipe is the only way to make an ffmpeg fed
	// by a pipe finish; it also guarantees the caller's pipe is not leaked.
	r.closeInput()

	if r.stdin != nil {
		if r.controlStdin {
			_ = writeWithTimeout(r.stdin, []byte(quitCommand))
		}
		// EOF, either after "q" or by itself, is a graceful quit request.
		r.closeStdin()
	}

	// The process may already have finished on its own: stopping must then
	// neither error nor block.
	select {
	case <-r.done:
		r.closeOutput()
		r.releaseProgress()
		return nil
	default:
	}

	// If Run is still between publishing the run and starting the process,
	// wait for that hand-off so the process is actually stopped.
	select {
	case <-r.done:
		r.closeOutput()
		r.releaseProgress()
		return nil
	case <-r.started:
	case <-time.After(stopKillWait):
		r.closeOutput()
		r.releaseProgress()
		return nil
	}

	if r.process == nil || r.process.Process == nil {
		// A run without an OS process (pipes injected for testing): release
		// the progress stream and the pipes.
		r.closeOutput()
		r.releaseProgress()
		return nil
	}

	// Give ffmpeg a chance to quit gracefully, keeping the output pipe open so
	// already produced data is not truncated.
	grace := time.NewTimer(stopGracePeriod)
	defer grace.Stop()

	select {
	case <-r.done:
		r.closeOutput()
		r.releaseProgress()
		return nil
	case <-grace.C:
	}

	// The process ignored the quit request; force it down.
	var stopErr error
	if err := r.process.Process.Kill(); err != nil && !isProcessDone(err) {
		stopErr = err
	}

	// An abandoned output consumer would keep the output pump (and the child)
	// blocked; closing the writer releases it.
	r.closeOutput()

	killWait := time.NewTimer(stopKillWait)
	defer killWait.Stop()

	select {
	case <-r.done:
	case <-killWait.C:
	}

	// Guarantee the progress channel unblocks even if the process could not be
	// reaped in time.
	r.closeOutput()
	r.releaseProgress()

	if stopErr != nil {
		return fmt.Errorf("failed to stop ffmpeg process: %w", stopErr)
	}
	return nil
}

// Output Returns the transcoding progress channel.
//
// The returned channel is closed exactly once: when the progress stream ends
// or when Stop is called. Calling Output more than once for the same run
// returns the same channel.
func (t *Transcoder) Output() <-chan Progress {
	t.mu.Lock()
	run := t.currentRun
	if run != nil {
		if run.output != nil {
			run.outTaken.Store(true)
			t.mu.Unlock()
			return run.output
		}
		t.mu.Unlock()

		// Progress was not requested for this run: preserve the historical
		// behaviour of a single, empty update on a closed channel.
		return closedProgressChannel(Progress{})
	}

	// No run was started through Run. Fall back to the manually injected
	// stderr pipe (SetProcessStderrPipe) so that path keeps working. The
	// reader is created once: subsequent Output calls return the same channel
	// instead of starting a competing reader on the same stream.
	var (
		startReader bool
		pipe        io.ReadCloser
	)
	t.manualOnce.Do(func() {
		t.manualPipe = t.stdErrPipe
		t.manualOutput = make(chan Progress)
		t.manualStop = make(chan struct{})
		t.manualReaderDone = make(chan struct{})
		pipe = t.manualPipe
		startReader = true
	})
	out := t.manualOutput
	stop := t.manualStop
	readerDone := t.manualReaderDone
	t.mu.Unlock()

	// The reader must not start while t.mu is held: parsing a progress line
	// reads the media metadata, which locks the same mutex.
	if startReader {
		var onDone func()
		if pipe != nil {
			onDone = func() { _ = pipe.Close() }
		}
		go t.readProgress(pipe, stop, out, onDone, func() { close(readerDone) })
	}

	return out
}

// stopManualProgress releases the progress stream used when Output was called
// without a Run. The reader may be blocked reading the caller-injected pipe,
// so the pipe itself is closed; the stop signal then lets the reader finish as
// soon as it regains control.
func stopManualProgress(stop chan struct{}, pipe io.ReadCloser, readerDone <-chan struct{}) {
	if stop != nil {
		close(stop)
	}
	if pipe != nil {
		_ = pipe.Close()
	}
	if readerDone != nil {
		select {
		case <-readerDone:
		case <-time.After(stopKillWait):
		}
	}
}

func closedProgressChannel(update Progress) <-chan Progress {
	out := make(chan Progress, 1)
	out <- update
	close(out)
	return out
}

// readProgress scans ffmpeg progress output and forwards parsed updates. It is
// the only goroutine that closes out.
func (t *Transcoder) readProgress(pipe io.ReadCloser, stop <-chan struct{}, out chan Progress, onDone, onReaderDone func()) {
	// Deferred calls run last-in-first-out: onReaderDone fires after the
	// channel is closed, so a waiter woken by it is guaranteed to observe the
	// closed channel.
	if onDone != nil {
		defer onDone()
	}
	if onReaderDone != nil {
		defer onReaderDone()
	}
	defer close(out)

	if pipe == nil {
		select {
		case out <- Progress{}:
		case <-stop:
		}
		return
	}

	scanner := bufio.NewScanner(pipe)
	scanner.Split(progressSplitFunc)

	buf := make([]byte, 2)
	scanner.Buffer(buf, bufio.MaxScanTokenSize)

	for scanner.Scan() {
		select {
		case <-stop:
			// Stop was requested: do not keep a consumer waiting.
			return
		default:
		}

		progress, ok := t.parseProgressLine(scanner.Text())
		if !ok {
			continue
		}

		select {
		case out <- progress:
			// Delivered.
		case <-stop:
			// Stop was requested: abandon the update rather than making the
			// consumer wait for a run that is being torn down.
			return
		}
	}
}

var progressFieldSpacesRegexp = regexp.MustCompile(`=\s+`)

// parseProgressLine parses a single ffmpeg progress line. It reports false for
// lines that are not progress lines.
func (t *Transcoder) parseProgressLine(line string) (Progress, bool) {
	if !strings.Contains(line, "frame=") || !strings.Contains(line, "time=") || !strings.Contains(line, "bitrate=") {
		return Progress{}, false
	}

	var progress Progress

	st := progressFieldSpacesRegexp.ReplaceAllString(line, `=`)
	f := strings.Fields(st)

	var framesProcessed string
	var currentTime string
	var currentBitrate string
	var currentSpeed string

	for j := 0; j < len(f); j++ {
		field := f[j]
		fieldSplit := strings.SplitN(field, "=", 2)
		if len(fieldSplit) != 2 {
			continue
		}

		fieldname := fieldSplit[0]
		fieldvalue := fieldSplit[1]

		switch fieldname {
		case "frame":
			framesProcessed = fieldvalue
		case "time":
			currentTime = fieldvalue
		case "bitrate":
			currentBitrate = fieldvalue
		case "speed":
			currentSpeed = fieldvalue
		}
	}

	timesec := duration.DurToSec(currentTime)

	var dursec float64
	if mediaFile := t.MediaFile(); mediaFile != nil {
		dursec, _ = strconv.ParseFloat(mediaFile.Metadata().Format.Duration, 64)
	}

	// Live streams report a zero (or missing) total duration: there is no
	// meaningful percentage to compute, and dividing by zero must be avoided.
	if dursec > 0 {
		progress.Progress = (timesec * 100) / dursec
	}

	progress.CurrentBitrate = currentBitrate
	progress.FramesProcessed = framesProcessed
	progress.CurrentTime = currentTime
	progress.Speed = currentSpeed

	return progress, true
}

// progressSplitFunc splits ffmpeg's stderr into lines. ffmpeg uses '\r' for
// progress updates (and "\r\n" on Windows), so both terminators are handled.
func progressSplitFunc(data []byte, atEOF bool) (advance int, token []byte, spliterror error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	// windows \r\n
	// so first \r and then \n can remove unexpected line break
	if i := bytes.IndexByte(data, '\r'); i >= 0 {
		// We have a cr terminated line
		return i + 1, data[0:i], nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, data[0:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}

// writeWithTimeout writes data without letting a blocked writer stall Stop.
func writeWithTimeout(w io.Writer, data []byte) error {
	errCh := make(chan error, 1)
	go func() {
		_, err := w.Write(data)
		errCh <- err
	}()

	timer := time.NewTimer(stopWriteTimeout)
	defer timer.Stop()

	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		return nil
	}
}

func isProcessDone(err error) bool {
	// Kill on an already reaped process reports os.ErrProcessDone; that is a
	// successful outcome for Stop, not a failure.
	return errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, os.ErrProcessDone)
}

func closeMediaPipes(mediaFile *media.File) {
	if mediaFile == nil {
		return
	}
	if mediaFile.InputPipe() && mediaFile.InputPipeReader() != nil {
		_ = mediaFile.InputPipeReader().Close()
	}
	if mediaFile.OutputPipe() && mediaFile.OutputPipeWriter() != nil {
		_ = mediaFile.OutputPipeWriter().Close()
	}
}
