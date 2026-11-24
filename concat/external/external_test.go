package external_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/concat/external"
	"github.com/SladkyCitron/gotau/umath"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sampleFormat = afmt.SampleFormat{BitDepth: 32, Encoding: afmt.SampleEncodingFloat, Endian: binary.LittleEndian}

var audioFormat = afmt.Format{SampleRate: 44100 * freq.Hertz, NumChannels: 1}

// TestMain allows the test binary to act as a fake external wavtool
// subprocess when GOTAU_TEST_HELPER=wavtool is set.
func TestMain(m *testing.M) {
	if os.Getenv("GOTAU_TEST_HELPER") == "wavtool" {
		helperWavtoolMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperWavtoolMain is a fake UTAU wavtool command. Its behavior is
// controlled by the GOTAU_TEST_HELPER_MODE environment variable.
func helperWavtoolMain() {
	tailPath, notePath := os.Args[1], os.Args[2]
	mode := os.Getenv("GOTAU_TEST_HELPER_MODE")
	switch mode {
	case "ok", "whddat":
		// "concatenate" by appending the note samples to the tail samples
		tail, err := decodeWavFile(tailPath)
		if err != nil {
			os.Exit(2)
		}
		note, err := decodeWavFile(notePath)
		if err != nil {
			os.Exit(2)
		}
		data, err := encodeWavBytes(append(tail, note...))
		if err != nil {
			os.Exit(2)
		}
		if mode == "ok" {
			if err := os.WriteFile(tailPath, data, 0666); err != nil {
				os.Exit(2)
			}
		} else {
			// some wavtools output whd/dat fragments instead of a wav file
			extless := tailPath[:len(tailPath)-len(filepath.Ext(tailPath))]
			if err := os.WriteFile(extless+".whd", data, 0666); err != nil {
				os.Exit(2)
			}
			if err := os.WriteFile(extless+".dat", nil, 0666); err != nil {
				os.Exit(2)
			}
			if err := os.Remove(tailPath); err != nil {
				os.Exit(2)
			}
		}
	case "fail":
		os.Exit(3)
	case "nooutput":
		// exit successfully without producing any output
		if err := os.Remove(tailPath); err != nil {
			os.Exit(2)
		}
	case "corrupt":
		if err := os.WriteFile(tailPath, []byte("not a wav file"), 0666); err != nil {
			os.Exit(2)
		}
	}
}

func decodeWavFile(path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec, err := wav.NewDecoder(f)
	if err != nil {
		return nil, err
	}
	return aio.ReadAll(dec)
}

func encodeWavBytes(samples []float32) ([]byte, error) {
	// encode via a real file: wav encoders patch the header sizes on Close,
	// which requires a seekable writer
	f, err := os.CreateTemp("", "gotau-test-helper-*.wav")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	enc, err := wav.NewEncoder(f, audioFormat, sampleFormat, wav.FormatFloat)
	if err != nil {
		return nil, err
	}
	if _, err := enc.WriteSamples(samples); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(f.Name())
}

func newTestConcatenator(mode string) *external.Concatenator {
	c := external.New(os.Args[0], sampleFormat)
	c.ConfigureCmd = func(cmd *exec.Cmd) {
		cmd.Env = append(os.Environ(),
			"GOTAU_TEST_HELPER=wavtool",
			"GOTAU_TEST_HELPER_MODE="+mode,
		)
	}
	return c
}

func testConcatConfig() concat.ConcatenateConfig {
	return concat.ConcatenateConfig{
		Duration:   480,
		Tempo:      120,
		Resolution: 480,
		Envelope: umath.Envelope{
			{X: 0, Y: 0},
			{X: 10, Y: 100},
			{X: 20, Y: 100},
			{X: 30, Y: 0},
		},
		AudioFormat: audioFormat,
	}
}

// tempResidue returns the temporary files left behind by the external
// concatenator in the system temporary directory.
func tempResidue(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "gotau-externalconcat-*"))
	require.NoError(t, err)
	return matches
}

func requireNoNewResidue(t *testing.T, before []string) {
	t.Helper()
	assert.ElementsMatch(t, before, tempResidue(t), "temporary files were left behind")
}

func TestConcatenateSuccess(t *testing.T) {
	before := tempResidue(t)

	tail := []float32{0.1, 0.2, 0.3}
	note := []float32{0.4, 0.5}
	out, err := newTestConcatenator("ok").Concatenate(tail, note, testConcatConfig())
	require.NoError(t, err)
	assert.Equal(t, []float32{0.1, 0.2, 0.3, 0.4, 0.5}, out)
	requireNoNewResidue(t, before)
}

// TestConcatenateWhdDat verifies the whd/dat output path used by some
// wavtools, including the cleanup of the fragment files.
func TestConcatenateWhdDat(t *testing.T) {
	before := tempResidue(t)

	tail := []float32{0.1, 0.2, 0.3}
	note := []float32{0.4, 0.5}
	out, err := newTestConcatenator("whddat").Concatenate(tail, note, testConcatConfig())
	require.NoError(t, err)
	assert.Equal(t, []float32{0.1, 0.2, 0.3, 0.4, 0.5}, out)
	requireNoNewResidue(t, before)
}

func TestConcatenateCommandFailure(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestConcatenator("fail").Concatenate([]float32{0.1}, []float32{0.2}, testConcatConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

func TestConcatenateMissingOutput(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestConcatenator("nooutput").Concatenate([]float32{0.1}, []float32{0.2}, testConcatConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

func TestConcatenateCorruptOutput(t *testing.T) {
	before := tempResidue(t)

	_, err := newTestConcatenator("corrupt").Concatenate([]float32{0.1}, []float32{0.2}, testConcatConfig())
	require.Error(t, err)
	requireNoNewResidue(t, before)
}

// TestConcatenateConcurrentSameNote verifies that concurrent concatenation
// requests with identical inputs don't clobber each other's temporary files.
func TestConcatenateConcurrentSameNote(t *testing.T) {
	before := tempResidue(t)

	tail := []float32{0.1, 0.2, 0.3}
	note := []float32{0.4, 0.5}
	want := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	c := newTestConcatenator("ok")
	cfg := testConcatConfig()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := c.Concatenate(tail, note, cfg)
			if err != nil {
				errs <- err
				return
			}
			if !slices.Equal(out, want) {
				errs <- fmt.Errorf("got samples %v, want %v", out, want)
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
