package mpegts

import (
	"bytes"
	"testing"
)

// buildPSIPacket creates a single MPEG-TS packet that carries a complete PSI
// section in its payload (pointer_field = 0).
func buildPSIPacket(t *testing.T, pid uint16, section []byte) []byte {
	t.Helper()
	payload := make([]byte, 0, 1+len(section))
	payload = append(payload, 0x00) // pointer_field
	payload = append(payload, section...)
	return buildPacket(t, pid, true, payload)
}

func buildPacket(t *testing.T, pid uint16, pusi bool, payload []byte) []byte {
	t.Helper()
	buf := make([]byte, PacketLen)
	pkt := CreatePacket(pid).WithPUSI(pusi).WithPayload(payload)
	if err := pkt.ToBytes(buf); err != nil {
		t.Fatalf("cannot build MPEG-TS packet: %v", err)
	}
	return buf
}

// syntheticPAT returns a PAT that maps program 1 to pmtPID.
func syntheticPAT(pmtPID uint16) []byte {
	section := []byte{
		0x00,       // table_id PAT
		0xb0, 0x0d, // section_length = 13
		0x00, 0x01, // transport_stream_id
		0xc1,       // version 0, current_next 1
		0x00, 0x00, // section_number, last_section_number
		0x00, 0x01, // program_number 1
		byte(0xe0 | pmtPID>>8), byte(pmtPID), // program_map_PID
		0x00, 0x00, 0x00, 0x00, // CRC32 (not validated)
	}
	return section
}

// syntheticPMT returns a PMT that maps elementary PID 0x0100 to H.264.
func syntheticPMT() []byte {
	section := []byte{
		0x02,       // table_id PMT
		0xb0, 0x12, // section_length = 18
		0x00, 0x01, // program_number 1
		0xc1,       // version 0, current_next 1
		0x00, 0x00, // section_number, last_section_number
		0xe1, 0x00, // PCR_PID 0x0100
		0xf0, 0x00, // program_info_length = 0
		0x1b,       // stream_type H.264
		0xe1, 0x00, // elementary_PID 0x0100
		0xf0, 0x00, // ES_info_length = 0
		0x00, 0x00, 0x00, 0x00, // CRC32 (not validated)
	}
	return section
}

// syntheticIDRPES returns a PES packet that starts with an H.264 SPS, which is
// everything the parser needs to report a decoder initialization point.
func syntheticIDRPES() []byte {
	payload := []byte{
		0x00, 0x00, 0x01, 0xe0, // PES start code, video stream
		0x00, 0x00, 0x80, 0x80, 0x05, // PES header
		0x21, 0x00, 0x01, 0x00, 0x01, // PTS/DTS
		0x00, 0x00, 0x00, 0x01, 0x67, // Annex B start code + SPS NAL
		0x42, 0x00, 0x1e, 0x95, 0xa8, 0x28, 0x0f, 0x00, 0x44, 0xfc, 0xb8, 0x08,
		0x80, // some SPS payload, content is irrelevant for the parser
	}
	return payload
}

// syntheticStream builds a three packet MPEG-TS stream locally: PAT, PMT and a
// PES packet with the H.264 SPS. No external tools or sample files are needed.
func syntheticStream(t *testing.T) ([]byte, [][]byte) {
	t.Helper()
	packets := [][]byte{
		buildPSIPacket(t, PIDPAT, syntheticPAT(0x1000)),
		buildPSIPacket(t, 0x1000, syntheticPMT()),
		buildPacket(t, 0x0100, true, syntheticIDRPES()),
	}
	var stream []byte
	for _, pkt := range packets {
		stream = append(stream, pkt...)
	}
	return stream, packets
}

// TestParser_SyntheticMpegTs verifies the parser on locally constructed
// MPEG-TS data: a PAT/PMT/PES triple must yield a full decoder init.
func TestParser_SyntheticMpegTs(t *testing.T) {
	stream, packets := syntheticStream(t)

	p := NewParser()
	for i, pkt := range packets {
		if err := p.Parse(pkt); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}

	init, err := p.InitData()
	if err != nil {
		t.Fatal(err)
	}
	if len(init) != 3 {
		t.Fatalf("expected PAT, PMT and SPS packet as init data, got %d entries", len(init))
	}
	for i, want := range packets {
		if !bytes.Equal(init[i], want) {
			t.Fatalf("init entry %d differs from the parsed packet", i)
		}
	}
	// Entry three is the whole TS packet, its payload starts after the header.
	if !bytes.HasPrefix(init[2][HeaderLen:], []byte{0x00, 0x00, 0x01, 0xe0}) {
		t.Fatalf("third init entry is not the PES packet: %x", init[2][HeaderLen:HeaderLen+4])
	}

	// The stream as a whole must also parse in one go.
	whole := NewParser()
	if err := whole.Parse(stream); err != nil {
		t.Fatal(err)
	}
	if got, _ := whole.InitData(); len(got) == 0 {
		t.Fatal("stream parsed as a whole yields no init data")
	}
}

// TestParser_InitDataDoesNotAliasInput pins the copy semantics of the parser:
// the relay recycles buffers as soon as a subscriber released them, so the
// parser must never hand out slices of caller-owned memory.
func TestParser_InitDataDoesNotAliasInput(t *testing.T) {
	stream, packets := syntheticStream(t)

	// Parse from a single mutable buffer, like the relay does with pooled
	// buffers that are reused for the next publisher read.
	buf := append([]byte(nil), stream...)
	p := NewParser()
	if err := p.Parse(buf); err != nil {
		t.Fatal(err)
	}
	init, err := p.InitData()
	if err != nil || len(init) != 3 {
		t.Fatalf("unexpected init data: %v %v", init, err)
	}
	want := make([][]byte, len(init))
	for i := range init {
		want[i] = append([]byte(nil), init[i]...)
	}

	// Simulate the buffer being recycled: overwrite everything.
	for i := range buf {
		buf[i] = 0xff
	}

	init, err = p.InitData()
	if err != nil {
		t.Fatal(err)
	}
	for i := range init {
		if !bytes.Equal(init[i], want[i]) {
			t.Fatalf("init entry %d aliases the caller buffer: got %x, want %x", i, init[i], want[i])
		}
	}

	// Same for a packet-by-packet feed with per-packet buffers.
	p = NewParser()
	for _, pkt := range packets {
		scratch := append([]byte(nil), pkt...)
		if err := p.Parse(scratch); err != nil {
			t.Fatal(err)
		}
		for i := range scratch {
			scratch[i] = 0xff
		}
	}
	init, err = p.InitData()
	if err != nil || len(init) != 3 {
		t.Fatalf("unexpected init data after recycling: %v %v", init, err)
	}
	for i, pkt := range packets {
		if !bytes.Equal(init[i], pkt) {
			t.Fatalf("init entry %d was mutated by buffer reuse", i)
		}
	}
}
