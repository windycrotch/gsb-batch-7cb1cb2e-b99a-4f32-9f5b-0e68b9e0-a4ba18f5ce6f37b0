// Package external implements an external concatenator.
package external

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/internal/timeutil"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
)

var _ concat.Concatenator = (*Concatenator)(nil)

// Concatenator is a concatenator that uses an external command-line
// UTAU wavtool program to perform concatenation.
type Concatenator struct {
	// ConfigureCmd is an optional hook that allows configuring the
	// exec.Cmd before running it.
	ConfigureCmd func(cmd *exec.Cmd)

	cmdName   string
	sampleFmt afmt.SampleFormat
}

// New creates a new [Concatenator] with the given program name and
// sample format for encoding temporary WAV files for passing into the concatenator.
//
// The program should be a command-line UTAU wavtool (e.g. wavtool, wavtool-yawu, etc.)
// that accepts tail and note WAV file paths and
// other parameters as arguments and concatenates accordingly.
func New(name string, sampleFmt afmt.SampleFormat) *Concatenator {
	return &Concatenator{cmdName: name, sampleFmt: sampleFmt}
}

func (c *Concatenator) Concatenate(tail []float32, note []float32, cfg concat.ConcatenateConfig) ([]float32, error) {
	if len(cfg.Envelope) < 4 || len(cfg.Envelope) > 5 {
		return nil, errors.New("envelope curve must have 4 or 5 points")
	}

	tailWav, noteWav, err := c.createTempWav(tail, note, cfg)
	if err != nil {
		return nil, fmt.Errorf("external: failed to create temporary wav files: %w", err)
	}
	// the temporary files created by this call (including possible whd/dat
	// fragments produced by the wavtool) are always cleaned up,
	// whether the concatenation succeeds or fails
	defer func() { _ = os.Remove(tailWav) }()
	defer func() { _ = os.Remove(noteWav) }()

	whdPath, datPath := whdDatPaths(tailWav)
	defer func() { _ = os.Remove(whdPath) }()
	defer func() { _ = os.Remove(datPath) }()

	var length string
	// most wavtools have TPQN 480 hardcoded so if our TPQN is 480 we use the wavtool length syntax
	// and if it's anything other than 480 then we calculate the length here instead
	if cfg.Resolution == 480 {
		// ticks@bpm+-correction
		length = fmt.Sprintf("%d@%.0f%+.0f", cfg.Duration, cfg.Tempo, cfg.LengthDelta)
	} else {
		length = strconv.FormatFloat((timeutil.TicksToSeconds(cfg.Duration, cfg.Resolution, cfg.Tempo)*1000)+cfg.LengthDelta, 'f', -1, 64)
	}

	args := []string{
		tailWav,                                  // output file
		noteWav,                                  // input file
		strconv.FormatInt(int64(cfg.Offset), 10), // STP
		length,                                   // note length
		strconv.FormatInt(int64(cfg.Envelope[0].X), 10),     // p1
		strconv.FormatInt(int64(cfg.Envelope[1].X), 10),     // p2
		strconv.FormatInt(int64(cfg.Envelope[2].X), 10),     // p3
		strconv.FormatInt(int64(cfg.Envelope[0].Y*100), 10), // v1
		strconv.FormatInt(int64(cfg.Envelope[1].Y*100), 10), // v2
		strconv.FormatInt(int64(cfg.Envelope[2].Y*100), 10), // v3
		strconv.FormatInt(int64(cfg.Envelope[3].Y*100), 10), // v4
		strconv.FormatInt(int64(cfg.Overlap), 10),           // overlap
		strconv.FormatInt(int64(cfg.Envelope[3].X), 10),     // p4
	}
	if len(cfg.Envelope) >= 5 {
		args = append(
			args,
			strconv.FormatInt(int64(cfg.Envelope[4].X), 10),     // p5
			strconv.FormatInt(int64(cfg.Envelope[4].Y*100), 10), // v5
		)
	}
	cmd := exec.Command(c.cmdName, args...)
	if c.ConfigureCmd != nil {
		c.ConfigureCmd(cmd)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("external: failed to run wavtool command: %w", err)
	}

	if _, err := os.Stat(tailWav); err == nil {
		// wav path
		f, err := os.Open(tailWav)
		if err != nil {
			return nil, fmt.Errorf("external: failed to open temporary output wav: %w", err)
		}
		deco, err := wav.NewDecoder(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("external: failed to decode temporary output wav: %w", err)
		}
		out, err := aio.ReadAll(deco)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("external: failed to read temporary output wav: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("external: failed to close temporary output wav: %w", err)
		}
		return out, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("external: failed to stat temporary output wav: %w", err)
	}

	// whd + dat path
	whdFile, err := os.Open(whdPath)
	if err != nil {
		return nil, fmt.Errorf("external: failed to open temporary whd: %w", err)
	}
	datFile, err := os.Open(datPath)
	if err != nil {
		_ = whdFile.Close()
		return nil, fmt.Errorf("external: failed to open temporary dat: %w", err)
	}
	r := io.MultiReader(whdFile, datFile)
	deco, err := wav.NewDecoder(r)
	if err != nil {
		_ = whdFile.Close()
		_ = datFile.Close()
		return nil, fmt.Errorf("external: failed to decode output wav: %w", err)
	}
	out, err := aio.ReadAll(deco)
	if err != nil {
		_ = whdFile.Close()
		_ = datFile.Close()
		return nil, fmt.Errorf("external: failed to read output wav: %w", err)
	}
	if err := whdFile.Close(); err != nil {
		_ = datFile.Close()
		return nil, fmt.Errorf("external: failed to close temporary whd: %w", err)
	}
	if err := datFile.Close(); err != nil {
		return nil, fmt.Errorf("external: failed to close temporary dat: %w", err)
	}
	return out, nil
}

// whdDatPaths returns the whd and dat fragment paths for the given output wav path.
func whdDatPaths(wavPath string) (whd string, dat string) {
	extless := wavPath[:len(wavPath)-len(filepath.Ext(wavPath))]
	return extless + ".whd", extless + ".dat"
}

func (c *Concatenator) createTempWav(tail []float32, note []float32, cfg concat.ConcatenateConfig) (string, string, error) {
	// create uniquely-named temporary files so that concurrent renders
	// (of the same or different notes) never clobber each other's files
	tailPath, err := c.writeTempWav("gotau-externalconcat-tail-*.wav", tail, cfg)
	if err != nil {
		return "", "", err
	}

	notePath, err := c.writeTempWav("gotau-externalconcat-note-*.wav", note, cfg)
	if err != nil {
		_ = os.Remove(tailPath)
		return "", "", err
	}

	return tailPath, notePath, nil
}

func (c *Concatenator) writeTempWav(pattern string, samples []float32, cfg concat.ConcatenateConfig) (path string, err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path = f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	var wavFormat uint16
	switch c.sampleFmt.Encoding {
	case afmt.SampleEncodingInt, afmt.SampleEncodingUint:
		wavFormat = wav.FormatInt
	case afmt.SampleEncodingFloat:
		wavFormat = wav.FormatFloat
	default:
		return "", fmt.Errorf("invalid sample format: %s", c.sampleFmt.String())
	}

	enc, err := wav.NewEncoder(f, cfg.AudioFormat, c.sampleFmt, wavFormat)
	if err != nil {
		return "", err
	}
	if _, err := enc.WriteSamples(samples); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	return path, nil
}
