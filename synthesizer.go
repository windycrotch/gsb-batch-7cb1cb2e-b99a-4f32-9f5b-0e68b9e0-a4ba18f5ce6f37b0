package xm

import (
	"errors"

	"github.com/quasilyte/xm/xmfile"
)

// Synthesizer can be used to play individual XM notes.
//
// It is more efficient and convenient to use for this
// use case than a stream with a constant module re-loading.
//
// Experimental: synthesizer API may change in the near future.
type Synthesizer struct {
	stream       *Stream
	noteCompiler *moduleCompiler
}

type SynthesizerInstruments struct {
	instOnly    module
	numChannels int
}

// GetInstrumentMapping returns a mapping of an actual compiled
// instrument id to the index from the source XM module.
// They can be identical and the mapping would look like x=>x,
// but if some instruments were invalid or empty, the mapping
// will not be 1-to-1.
func (iset *SynthesizerInstruments) GetInstrumentMapping() []int {
	num := 0
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			num++
		}
	}
	m := make([]int, 0, num)
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			m = append(m, inst.id)
		}
	}
	return m
}

type SynthesizerConfig struct {
	NumChannels int
}

func NewSynthesizer(config SynthesizerConfig) *Synthesizer {
	stream := NewStream()
	stream.channels = make([]streamChannel, config.NumChannels)
	stream.activeChannels = make([]*streamChannel, config.NumChannels)
	return &Synthesizer{
		stream:       stream,
		noteCompiler: newSynthCompiler(),
	}
}

// SetVolume adjusts the global volume scaling for the underlying stream.
func (s *Synthesizer) SetVolume(v float64) {
	s.stream.SetVolume(v)
}

// SetInstruments loads the instruments from a list received from GetInstruments.
func (s *Synthesizer) SetInstruments(is *SynthesizerInstruments) error {
	if len(s.stream.channels) != is.numChannels {
		return errors.New("mismatching channel count")
	}

	// assignCompiledModule rewinds the stream and resets every
	// channel, so no state from the previous instruments
	// (or played notes) leaks into the next audition.
	s.stream.assignCompiledModule(is.instOnly)

	return nil
}

// GetInstruments returns the previously loaded instruments.
// It is intended to be used in combination with SetInstruments
// to avoid the need for repeated LoadInstruments for the
// same module. It is much more efficient as it avoids
// the module re-compilation.
// The instruments are only compatible if the number of channels is matching.
func (s *Synthesizer) GetInstruments() *SynthesizerInstruments {
	return &SynthesizerInstruments{
		instOnly:    s.stream.module,
		numChannels: len(s.stream.channels),
	}
}

// LoadInstruments prepares the instruments from the module
// for further use.
//
// Loading instruments involves module compilation,
// so it should not be called on a hot path repeatedly.
// If you need multiple modules at once, consider
// using several synthesizers, one per module.
//
// The patterns don't really matter as this method
// is only interested in instruments (and samples).
func (s *Synthesizer) LoadInstruments(m *xmfile.Module, config LoadModuleConfig) error {
	s.stream.applyConfigDefaults(m, &config)

	instOnly, err := s.compileModuleInstruments(m, config)
	if err != nil {
		return err
	}

	s.stream.assignCompiledModule(instOnly)

	return nil
}

func (s *Synthesizer) compileModuleInstruments(m *xmfile.Module, config LoadModuleConfig) (module, error) {
	instOnly := xmfile.Module{
		Name:           m.Name,
		Version:        m.Version,
		NumInstruments: m.NumInstruments,
		Flags:          m.Flags,
		Instruments:    m.Instruments,
	}

	compiled, err := compileModule(&instOnly, moduleConfig{
		sampleRate: config.SampleRate,
		bpm:        config.BPM,
		tempo:      config.Tempo,
		subSamples: config.LinearInterpolation,
	})
	if err != nil {
		return compiled, err
	}

	compiled.patterns = []pattern{
		{
			numChannels: len(s.stream.channels),
			numRows:     1,
			notes:       make([]uint16, len(s.stream.channels)),
		},
	}
	compiled.patternOrder = []*pattern{
		&compiled.patterns[0],
	}

	pat := &compiled.patterns[0]
	for i := range pat.notes {
		pat.notes[i] = uint16(i)
	}

	return compiled, nil
}

// PlayNote plays one or more notes up to the specified duration.
// Using 0 for the duration will play it for several seconds.
//
// Every call starts the audition from a clean state: nothing
// (note, instrument, envelope frame, portamento target, vibrato
// phase or sample offset) is inherited from the previous audition.
//
// If a note can't be compiled, an error is returned and the
// previously prepared (still playable) state is left untouched.
func (s *Synthesizer) PlayNote(duration float64, notes ...xmfile.PatternNote) error {
	numChannels := len(s.stream.channels)

	tempo := 240
	if duration != 0 {
		tempo = 1 + int(ticksPerSecond(s.stream.bpm)*duration)
	}

	// Compile the notes into temporary tables first.
	// If anything goes wrong, the stream state is not modified
	// and the previous note can still be played.
	tmp := s.stream.module
	tmp.ticksPerRow = tempo
	tmp.noteTab = make([]patternNote, 0, numChannels)
	tmp.effectTab = make([]noteEffect, 0, 8)
	s.noteCompiler.reset(&tmp)

	chansUsed := 0
	for _, note := range notes {
		n, _, err := s.noteCompiler.compileNote(note)
		if err != nil {
			return err
		}
		tmp.noteTab = append(tmp.noteTab, n)
		chansUsed++
		if chansUsed >= numChannels {
			break
		}
	}

	for chansUsed < numChannels {
		tmp.noteTab = append(tmp.noteTab, patternNote{})
		chansUsed++
	}

	// Commit the new note data and restart the playback from
	// a clean state, so this audition inherits nothing from
	// the previous one. The fresh tables are also never shared
	// with SynthesizerInstruments snapshots or other streams.
	s.stream.module.noteTab = tmp.noteTab
	s.stream.module.effectTab = tmp.effectTab
	s.stream.setTempo(tempo)
	s.stream.rewind()

	return nil
}

func (s *Synthesizer) Read(b []byte) (int, error) {
	return s.stream.Read(b)
}

func (s *Synthesizer) Rewind() {
	s.stream.Rewind()
}

func (s *Synthesizer) Seek(offset int64, whence int) (int64, error) {
	return s.stream.Seek(offset, whence)
}
