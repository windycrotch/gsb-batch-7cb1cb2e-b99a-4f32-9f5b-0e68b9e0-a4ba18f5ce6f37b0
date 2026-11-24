package xmfile

import (
	"bytes"
	"encoding/binary"
	"testing"
)

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

func TestParseMultipleModulesAreIndependent(t *testing.T) {
	p := NewParser(ParserConfig{})

	m1, err := p.ParseFromBytes(buildTestXM(48))
	if err != nil {
		t.Fatal(err)
	}
	m2, err := p.ParseFromBytes(buildTestXM(60))
	if err != nil {
		t.Fatal(err)
	}

	// Parsing the second module must not invalidate the first one.
	if len(m1.Notes) < 2 {
		t.Fatalf("m1: unexpected notes count: %d", len(m1.Notes))
	}
	if m1.Notes[1].Note != 48 {
		t.Errorf("m1 note changed after parsing m2: got %d, want 48", m1.Notes[1].Note)
	}
	if len(m2.Notes) < 2 {
		t.Fatalf("m2: unexpected notes count: %d", len(m2.Notes))
	}
	if m2.Notes[1].Note != 60 {
		t.Errorf("m2 note: got %d, want 60", m2.Notes[1].Note)
	}

	if len(m1.Patterns) != 1 || len(m1.Patterns[0].Rows) != 1 {
		t.Fatalf("m1: unexpected patterns data")
	}
	if m1.Patterns[0].Rows[0].Notes[0] != 1 {
		t.Errorf("m1 pattern data changed after parsing m2")
	}
	if len(m1.Instruments) != 1 || len(m1.Instruments[0].Samples) != 1 {
		t.Fatalf("m1: unexpected instruments data")
	}
	if m1.Instruments[0].Samples[0].Length != 8 {
		t.Errorf("m1 sample data changed after parsing m2")
	}

	// Modules parsed separately must be identical to the
	// modules parsed with a shared parser.
	m1solo, err := NewParser(ParserConfig{}).ParseFromBytes(buildTestXM(48))
	if err != nil {
		t.Fatal(err)
	}
	if m1solo.Notes[1].Note != m1.Notes[1].Note {
		t.Errorf("shared-parser module differs from the solo-parser module")
	}
}
