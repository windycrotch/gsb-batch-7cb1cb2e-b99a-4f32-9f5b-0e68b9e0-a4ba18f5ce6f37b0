package pipeline_test

import (
	"io/ioutil"
	"math"
	"os"
	"path/filepath"

	"github.com/sfluor/musig/internal/pkg/fingerprint"
	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/sfluor/musig/pkg/dsp"
	"github.com/youpy/go-wav"
)

const (
	testSampleRate = 44100
	testDurationS  = 2
)

// writeTestWAV synthesizes a short mono 16-bit WAV file made of the given
// sine wave frequencies and writes it at the given path. Passing several
// frequencies produces a richer constellation map (multiple frequency bands
// active at every time step), which is what makes matching scores meaningful.
func writeTestWAV(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, path string, freqs ...float64) {
	t.Helper()
	if len(freqs) == 0 {
		freqs = []float64{440, 880}
	}

	n := testSampleRate * testDurationS
	samples := make([]wav.Sample, n)
	for i := range samples {
		var v float64
		for _, f := range freqs {
			v += math.Sin(2 * math.Pi * f * float64(i) / testSampleRate)
		}
		// Scale into the int16 range, attenuated so the sum never clips.
		samples[i] = wav.Sample{Values: [2]int{int(v * 12000 / float64(len(freqs))), 0}}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("could not create wav file %s: %v", path, err)
	}
	defer f.Close()

	w := wav.NewWriter(f, uint32(n), 1, uint32(testSampleRate), 16)
	if err := w.WriteSamples(samples); err != nil {
		t.Fatalf("could not write wav samples: %v", err)
	}
}

// fingerprintWAV computes the fingerprint map (with placeholder song ID 0)
// that the pipeline would store for the given WAV file.
func fingerprintWAV(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, path string) map[model.EncodedKey]model.TableValue {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("could not open wav: %v", err)
	}
	defer f.Close()

	s := dsp.NewSpectrogrammer(model.DownsampleRatio, model.MaxFreq, model.SampleSize, true)
	spec, spr, err := s.Spectrogram(f)
	if err != nil {
		t.Fatalf("could not build spectrogram: %v", err)
	}
	cMap := s.ConstellationMap(spec, spr)

	return fingerprint.NewDefaultFingerprinter().Fingerprint(0, cMap)
}

func mustTempDir(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, prefix string) string {
	t.Helper()
	dir, err := ioutil.TempDir("", prefix)
	if err != nil {
		t.Fatalf("could not create temp dir: %v", err)
	}
	return dir
}

func mustWriteFile(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, path, content string) {
	t.Helper()
	if err := ioutil.WriteFile(path, []byte(content), 0666); err != nil {
		t.Fatalf("could not write file %s: %v", path, err)
	}
}

func join(parts ...string) string {
	return filepath.Join(parts...)
}

func newTestSpectrogrammer() *dsp.Spectrogrammer {
	return dsp.NewSpectrogrammer(model.DownsampleRatio, model.MaxFreq, model.SampleSize, true)
}

func newTestFingerprinter() fingerprint.Fingerprinter {
	return fingerprint.NewDefaultFingerprinter()
}
