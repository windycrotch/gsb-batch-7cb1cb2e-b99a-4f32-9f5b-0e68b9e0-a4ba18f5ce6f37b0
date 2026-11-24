package xm

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/quasilyte/xm/xmfile"
)

// makeTestModule builds a small but valid module directly,
// without involving the XM parser.
// The module has 2 channels, 1 pattern of 4 rows and a single
// instrument with a forward-looped sample.
func makeTestModule(note uint8) *xmfile.Module {
	return &xmfile.Module{
		Name:           "test module",
		Version:        [2]byte{1, 4},
		SongLength:     1,
		NumChannels:    2,
		NumPatterns:    1,
		NumInstruments: 1,
		Flags:          1, // Linear frequency table
		DefaultTempo:   6,
		DefaultBPM:     125,
		PatternOrder:   []uint8{0},
		Notes: []xmfile.PatternNote{
			{ID: 0},
			{ID: 1, Note: note, Instrument: 1},
		},
		Patterns: []xmfile.Pattern{
			{Rows: []xmfile.PatternRow{
				{Notes: []uint16{1, 0}},
				{Notes: []uint16{0, 0}},
				{Notes: []uint16{0, 0}},
				{Notes: []uint16{0, 0}},
			}},
		},
		Instruments: []xmfile.Instrument{
			{
				Samples: []xmfile.InstrumentSample{
					{
						Length:     8,
						LoopStart:  0,
						LoopLength: 8,
						Volume:     64,
						Panning:    128,
						TypeFlags:  1, // Forward loop
						Data:       []uint8{8, 8, 0xf8, 0xf8, 8, 8, 0xf8, 0xf8},
					},
				},
			},
		},
	}
}

// buildTestXM creates a raw XM file with a single pattern
// that plays the given note on channel 0.
func buildTestXM(note uint8) []byte {
	buf := &bytes.Buffer{}
	w := func(v any) {
		if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	pad := func(n int) {
		buf.Write(make([]byte, n))
	}

	buf.WriteString("Extended Module: ")
	name := make([]byte, 20)
	copy(name, "test")
	buf.Write(name)
	buf.WriteByte(0x1a)
	pad(20) // tracker name
	w(uint16(0x0104))
	w(uint32(21))    // header size: 4 + 8*2 + 1 (pattern order)
	w(uint16(1))     // song length
	w(uint16(0))     // restart position
	w(uint16(2))     // num channels
	w(uint16(1))     // num patterns
	w(uint16(1))     // num instruments
	w(uint16(1))     // flags: linear
	w(uint16(6))     // default tempo
	w(uint16(125))   // default bpm
	buf.WriteByte(0) // pattern order

	// The only pattern: 1 row, note on channel 0, empty channel 1.
	w(uint32(9)) // pattern header length
	buf.WriteByte(0)
	w(uint16(1)) // num rows
	w(uint16(6)) // packed data size
	buf.Write([]byte{note, 1, 0, 0, 0})
	buf.WriteByte(0x80)

	// The only instrument with a single 8-bit sample.
	w(uint32(241)) // instrument header size
	pad(22)        // name
	buf.WriteByte(0)
	w(uint16(1))     // num samples
	w(uint32(40))    // sample header size
	pad(96)          // keymap assignments
	pad(48)          // volume envelope points
	pad(48)          // panning envelope points
	buf.WriteByte(0) // num volume points
	buf.WriteByte(0) // num panning points
	pad(6)           // sustain/loop points
	buf.WriteByte(0) // volume type
	buf.WriteByte(0) // panning type
	pad(4)           // vibrato
	w(uint16(0))     // volume fadeout

	// Sample header.
	w(uint32(8))       // length
	w(uint32(0))       // loop start
	w(uint32(8))       // loop length
	buf.WriteByte(64)  // volume
	buf.WriteByte(0)   // finetune
	buf.WriteByte(1)   // type: forward loop
	buf.WriteByte(128) // panning
	buf.WriteByte(0)   // relative note
	buf.WriteByte(0)   // format: delta packed
	pad(22)            // name

	// Sample data (delta encoded).
	buf.Write([]byte{8, 8, 0xf8, 0xf8, 8, 8, 0xf8, 0xf8})

	return buf.Bytes()
}

func readChunk(t *testing.T, r io.Reader, size int) ([]byte, error) {
	t.Helper()
	buf := make([]byte, size)
	n, err := r.Read(buf)
	return buf[:n], err
}

func checkStreamsEqual(t *testing.T, s1, s2 *Stream, numReads int) {
	t.Helper()
	for i := 0; i < numReads; i++ {
		b1, err1 := readChunk(t, s1, 8192)
		b2, err2 := readChunk(t, s2, 8192)
		if err1 != err2 {
			t.Fatalf("read %d: errors mismatch: %v vs %v", i, err1, err2)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("read %d: PCM data mismatch", i)
		}
	}
}

func TestLoadModuleResetsPlaybackState(t *testing.T) {
	m1 := makeTestModule(48)
	m2 := makeTestModule(60)

	s := NewStream()
	s.SetVolume(0.5)
	s.SetLooping(true)
	var events []StreamEvent
	s.SetEventHandler(func(e StreamEvent) {
		events = append(events, e)
	})
	if err := s.LoadModule(m1, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, s, 8192); err != nil {
		t.Fatal(err)
	}
	// Simulate the state a real playback would leave behind.
	s.globalVolume = 0.25
	s.jumpKind = jumpPatternBreak
	s.jumpPattern = 2
	s.jumpRow = 3

	if err := s.LoadModule(m2, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}

	// The caller-level settings must be preserved.
	if s.settings.volumeScaling != 0.5 {
		t.Errorf("volume scaling was not preserved: %v", s.settings.volumeScaling)
	}
	if !s.settings.loop {
		t.Errorf("looping setting was not preserved")
	}
	if s.settings.eventHandler == nil {
		t.Fatalf("event handler was not preserved")
	}

	// The playback state must be reset to the new module initial state.
	if s.bytePos != 0 {
		t.Errorf("bytePos is not reset: %d", s.bytePos)
	}
	if s.t != 0 {
		t.Errorf("playback time is not reset: %v", s.t)
	}
	if s.globalVolume != 1 {
		t.Errorf("global volume is not reset: %v", s.globalVolume)
	}
	if s.jumpKind != jumpNone {
		t.Errorf("jump state is not reset: %v", s.jumpKind)
	}
	if s.bpm != 125 {
		t.Errorf("bpm is not reset: %v", s.bpm)
	}
	if s.ticksPerRow != 6 {
		t.Errorf("tempo is not reset: %v", s.ticksPerRow)
	}
	for i := range s.channels {
		ch := &s.channels[i]
		if ch.inst != nil || ch.note != nil || ch.keyOn {
			t.Errorf("channel %d keeps the old note/instrument", i)
		}
		if ch.sampleOffset != 0 {
			t.Errorf("channel %d keeps the old sample offset", i)
		}
		if ch.volumeEnvelope.frame != 0 || ch.panningEnvelope.frame != 0 {
			t.Errorf("channel %d keeps the old envelope frame", i)
		}
		if ch.vibratoStep != 0 || ch.notePortamentoTargetPeriod != 0 {
			t.Errorf("channel %d keeps the old vibrato/portamento state", i)
		}
	}

	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("Seek(0, SeekCurrent) after reload: got %d, want 0", pos)
	}

	// The reloaded stream must produce exactly the same PCM as
	// a fresh stream that plays only the new module.
	ref := NewStream()
	ref.SetVolume(0.5)
	ref.SetLooping(true)
	if err := ref.LoadModule(m2, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	checkStreamsEqual(t, s, ref, 8)

	// The event handler must still be called after the reload.
	noteEvents := 0
	for _, e := range events {
		if e.Kind == EventNote {
			noteEvents++
		}
	}
	if noteEvents == 0 {
		t.Errorf("expected note events after the reload")
	}
}

func TestLoadModuleFailureKeepsOldModule(t *testing.T) {
	newBadModule := func() *xmfile.Module {
		bad := makeTestModule(60)
		bad.Flags = 0 // Amiga frequency table is not supported
		bad.NumChannels = 4
		return bad
	}

	s := NewStream()
	if err := s.LoadModule(makeTestModule(48), LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, s, 8192); err != nil {
		t.Fatal(err)
	}

	// A reference stream that never attempts to load the broken module.
	ref := NewStream()
	if err := ref.LoadModule(makeTestModule(48), LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, ref, 8192); err != nil {
		t.Fatal(err)
	}

	if err := s.LoadModule(newBadModule(), LoadModuleConfig{}); err == nil {
		t.Fatalf("expected a compilation error")
	}

	// The old module must continue to play as if nothing happened.
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	refPos, err := ref.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != refPos {
		t.Fatalf("byte pos changed after a failed reload: %d vs %d", pos, refPos)
	}
	checkStreamsEqual(t, s, ref, 4)
}

func TestStreamRewindResetsBytePos(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(makeTestModule(48), LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, s, 8192); err != nil {
		t.Fatal(err)
	}
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos == 0 {
		t.Fatalf("expected a non-zero byte pos after Read")
	}

	s.Rewind()

	pos, err = s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("Seek(0, SeekCurrent) after Rewind: got %d, want 0", pos)
	}

	// After a rewind, the stream must play from the beginning again.
	ref := NewStream()
	if err := ref.LoadModule(makeTestModule(48), LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	checkStreamsEqual(t, s, ref, 4)
}

func newTestSynthesizer(t *testing.T) *Synthesizer {
	t.Helper()
	s := NewSynthesizer(SynthesizerConfig{NumChannels: 2})
	if err := s.LoadInstruments(makeTestModule(0), LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func checkSynthesizersEqual(t *testing.T, s1, s2 *Synthesizer, numReads int) {
	t.Helper()
	for i := 0; i < numReads; i++ {
		b1, err1 := readChunk(t, s1, 8192)
		b2, err2 := readChunk(t, s2, 8192)
		if err1 != err2 {
			t.Fatalf("read %d: errors mismatch: %v vs %v", i, err1, err2)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("read %d: PCM data mismatch", i)
		}
	}
}

func TestSynthesizerPlayNoteStartsClean(t *testing.T) {
	note1 := xmfile.PatternNote{Note: 48, Instrument: 1}
	note2 := xmfile.PatternNote{Note: 60, Instrument: 1}

	s1 := newTestSynthesizer(t)
	if err := s1.PlayNote(0, note1); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, s1, 8192); err != nil {
		t.Fatal(err)
	}
	// The second audition must start from a clean state.
	if err := s1.PlayNote(0, note2); err != nil {
		t.Fatal(err)
	}
	for i := range s1.stream.channels {
		ch := &s1.stream.channels[i]
		if ch.inst != nil || ch.sampleOffset != 0 || ch.keyOn {
			t.Errorf("channel %d keeps state from the previous audition", i)
		}
		if ch.volumeEnvelope.frame != 0 || ch.vibratoStep != 0 || ch.notePortamentoTargetPeriod != 0 {
			t.Errorf("channel %d keeps envelope/vibrato/portamento state", i)
		}
	}

	// A reference synthesizer that plays only the second note.
	s2 := newTestSynthesizer(t)
	if err := s2.PlayNote(0, note2); err != nil {
		t.Fatal(err)
	}

	checkSynthesizersEqual(t, s1, s2, 4)
}

func TestSynthesizerPlayNoteFailurePreservesState(t *testing.T) {
	good := xmfile.PatternNote{Note: 48, Instrument: 1}
	// A volume slide with both up & down values fails to compile.
	bad := xmfile.PatternNote{Note: 48, Instrument: 1, EffectType: 0x0A, EffectParameter: 0xFF}

	s1 := newTestSynthesizer(t)
	if err := s1.PlayNote(0, good); err != nil {
		t.Fatal(err)
	}
	if err := s1.PlayNote(0, bad); err == nil {
		t.Fatalf("expected a note compilation error")
	}

	// The failed audition must not break the previously prepared note.
	s2 := newTestSynthesizer(t)
	if err := s2.PlayNote(0, good); err != nil {
		t.Fatal(err)
	}
	checkSynthesizersEqual(t, s1, s2, 4)
}

func TestSynthesizerSetInstrumentsResetsChannels(t *testing.T) {
	s := newTestSynthesizer(t)
	if err := s.PlayNote(0, xmfile.PatternNote{Note: 48, Instrument: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(t, s, 8192); err != nil {
		t.Fatal(err)
	}

	iset := s.GetInstruments()
	if err := s.SetInstruments(iset); err != nil {
		t.Fatal(err)
	}

	for i := range s.stream.channels {
		ch := &s.stream.channels[i]
		if ch.inst != nil || ch.note != nil || ch.keyOn {
			t.Errorf("channel %d keeps the old note/instrument", i)
		}
		if ch.sampleOffset != 0 {
			t.Errorf("channel %d keeps the old sample offset", i)
		}
		if ch.volumeEnvelope.frame != 0 || ch.panningEnvelope.frame != 0 {
			t.Errorf("channel %d keeps the old envelope frame", i)
		}
		if ch.vibratoStep != 0 || ch.notePortamentoTargetPeriod != 0 {
			t.Errorf("channel %d keeps the old vibrato/portamento state", i)
		}
	}
}

func TestSynthesizersSharingInstruments(t *testing.T) {
	note1 := xmfile.PatternNote{Note: 48, Instrument: 1}
	note2 := xmfile.PatternNote{Note: 60, Instrument: 1}

	s1 := newTestSynthesizer(t)
	if err := s1.PlayNote(0, note1); err != nil {
		t.Fatal(err)
	}
	iset := s1.GetInstruments()

	s2 := NewSynthesizer(SynthesizerConfig{NumChannels: 2})
	if err := s2.SetInstruments(iset); err != nil {
		t.Fatal(err)
	}
	if err := s2.PlayNote(0, note2); err != nil {
		t.Fatal(err)
	}

	// Playing a note on s2 must not corrupt the s1 note data.
	s1.Rewind()
	ref := newTestSynthesizer(t)
	if err := ref.PlayNote(0, note1); err != nil {
		t.Fatal(err)
	}
	checkSynthesizersEqual(t, s1, ref, 4)
}

func TestParserReuseProducesIndependentStreams(t *testing.T) {
	p := xmfile.NewParser(xmfile.ParserConfig{})
	m1, err := p.ParseFromBytes(buildTestXM(48))
	if err != nil {
		t.Fatal(err)
	}
	m2, err := p.ParseFromBytes(buildTestXM(60))
	if err != nil {
		t.Fatal(err)
	}

	// Loading both modules after both parses must give the same
	// results as loading independently parsed modules.
	s1 := NewStream()
	if err := s1.LoadModule(m1, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	s2 := NewStream()
	if err := s2.LoadModule(m2, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}

	m1ref, err := xmfile.NewParser(xmfile.ParserConfig{}).ParseFromBytes(buildTestXM(48))
	if err != nil {
		t.Fatal(err)
	}
	ref1 := NewStream()
	if err := ref1.LoadModule(m1ref, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}
	m2ref, err := xmfile.NewParser(xmfile.ParserConfig{}).ParseFromBytes(buildTestXM(60))
	if err != nil {
		t.Fatal(err)
	}
	ref2 := NewStream()
	if err := ref2.LoadModule(m2ref, LoadModuleConfig{}); err != nil {
		t.Fatal(err)
	}

	checkStreamsEqual(t, s1, ref1, 4)
	checkStreamsEqual(t, s2, ref2, 4)
}
