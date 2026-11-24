package external_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/gotau/resampler/external"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sampleFormat = afmt.SampleFormat{BitDepth: 32, Encoding: afmt.SampleEncodingFloat, Endian: binary.LittleEndian}

var audioFormat = afmt.Format{SampleRate: 44100 * freq.Hertz, NumChannels: 1}

// TestMain allows the test binary to act as a fake external resampler
// subprocess when GOTAU_TEST_HELPER=resampler is set.
func TestMain(m *testing.M) {
	if os.Getenv("GOTAU_TEST_HELPER") == "resampler" {
		helperResamplerMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperResamplerMain is a fake UTAU resampler command. Its behavior is
// controlled by the GOTAU_TEST_HELPER_MODE environment variable.
func helperResamplerMain() {
	input, output := os.Args[1], os.Args[2]
	switch os.Getenv("GOTAU_TEST_HELPER_MODE") {
	case "ok":
		// "resample" by copying the input wav to the output
		data, err := os.ReadFile(input)
		if err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(output, data, 0666); err != nil {
			os.Exit(2)
		}
	case "analyze":
		// produce the dummy output and the analysis sidecar file
		if err := os.WriteFile(output, []byte("dummy"), 0666); err != nil {
			os.Exit(2)
		}
		ext := filepath.Ext(input)
		extless := input[:len(input)-len(ext)]
		analysisPath := extless + strings.ReplaceAll(ext, ".", "_") + ".frq"
		if err := os.WriteFile(analysisPath, []byte("analysis-data"), 0666); err != nil {
			os.Exit(2)
		}
	case "fail":
		os.Exit(3)
	case "nooutput":
		// exit successfully without producing any output
	case "corrupt":
		if err := os.WriteFile(output, []byte("not a wav file"), 0666); err != nil {
			os.Exit(2)
		}
	}
}

func newTestResampler(mode string) *external.Resampler {
	r := external.New(os.Args[0], ".frq", sampleFormat)
	r.ConfigureCmd = func(cmd *exec.Cmd) {
		cmd.Env = append(os.Environ(),
			"GOTAU_TEST_HELPER=resampler",
			"GOTAU_TEST_HELPER_MODE="+mode,
		)
	}
	return r
}

func encodeWav(t *testing.T, samples []float32) []byte {
	t.Helper()
	// encode via a real file: wav encoders patch the header sizes on Close,
	// which requires a seekable writer
	f, err := os.CreateTemp(t.TempDir(), "samples-*.wav")
	require.NoError(t, err)
	defer f.Close()
	enc, err := wav.NewEncoder(f, audioFormat, sampleFormat, wav.FormatFloat)
	require.NoError(t, err)
	_, err = enc.WriteSamples(samples)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return data
}

func sampleReader(t *testing.T, samples []float32) aio.SampleReader {
	t.Helper()
	dec, err := wav.NewDecoder(bytes.NewReader(encodeWav(t, samples)))
	require.NoError(t, err)
	return dec
}

func testConfig() resampler.ResampleConfig {
	return resampler.ResampleConfig{
		Pitch:       60,
		Velocity:    100,
		Length:      250,
		Consonant:   80,
		Intensity:   1,
		Tempo:       120,
		AudioFormat: audioFormat,
	}
}

// tempResidue returns the temporary files left behind by the external
// resampler in the system temporary directory.
func tempResidue(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "gotau-externalresampler-*"))
	require.NoError(t, err)
	return matches
}

func requireNoNewResidue(t *testing.T, before []string) {
	t.Helper()
	assert.ElementsMatch(t, before, tempResidue(t), "temporary files were left behind")
}

func TestResampleSuccess(t *testing.T) {
	before := tempResidue(t)

	in := []float32{0.1, -0.2, 0.3, -0.4, 0.5}
	out, err := newTestResampler("ok").Resample(sampleReader(t, in), testConfig())
	require.NoError(t, err)
	samples, err := aio.ReadAll(out)
	require.NoError(t, err)
	assert.Equal(t, in, samples)
	requireNoNewResidue(t, before)
}

func TestResampleCommandFailure(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestResampler("fail").Resample(sampleReader(t, []float32{0.1}), testConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

func TestResampleMissingOutput(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestResampler("nooutput").Resample(sampleReader(t, []float32{0.1}), testConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

func TestResampleCorruptOutput(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestResampler("corrupt").Resample(sampleReader(t, []float32{0.1}), testConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

func TestResampleWithAnalysisSuccess(t *testing.T) {
	before := tempResidue(t)

	in := []float32{0.1, -0.2, 0.3}
	out, err := newTestResampler("ok").ResampleWithAnalysis(sampleReader(t, in), strings.NewReader("analysis"), testConfig())
	require.NoError(t, err)
	samples, err := aio.ReadAll(out)
	require.NoError(t, err)
	assert.Equal(t, in, samples)
	requireNoNewResidue(t, before)
}

func TestResampleWithAnalysisCommandFailure(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestResampler("fail").ResampleWithAnalysis(sampleReader(t, []float32{0.1}), strings.NewReader("analysis"), testConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

// TestResampleConcurrentSameNote verifies that concurrent resampling requests
// for the same note (identical configuration) don't clobber each other's
// temporary files.
func TestResampleConcurrentSameNote(t *testing.T) {
	before := tempResidue(t)

	in := []float32{0.1, -0.2, 0.3, -0.4}
	inData := encodeWav(t, in)
	r := newTestResampler("ok")
	cfg := testConfig()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec, err := wav.NewDecoder(bytes.NewReader(inData))
			if err != nil {
				errs <- err
				return
			}
			out, err := r.Resample(dec, cfg)
			if err != nil {
				errs <- err
				return
			}
			samples, err := aio.ReadAll(out)
			if err != nil {
				errs <- err
				return
			}
			if !slices.Equal(samples, in) {
				errs <- fmt.Errorf("got samples %v, want %v", samples, in)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	requireNoNewResidue(t, before)
}

func TestAnalyzeSuccess(t *testing.T) {
	before := tempResidue(t)

	r := newTestResampler("analyze")
	rc, err := r.Analyze(sampleReader(t, []float32{0.1, 0.2}), audioFormat)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "analysis-data", string(data))
	require.NoError(t, rc.Close())
	requireNoNewResidue(t, before)
}

func TestAnalyzeCommandFailure(t *testing.T) {
	before := tempResidue(t)

	r := newTestResampler("fail")
	_, err := r.Analyze(sampleReader(t, []float32{0.1}), audioFormat)
	require.Error(t, err)
	requireNoNewResidue(t, before)
}
