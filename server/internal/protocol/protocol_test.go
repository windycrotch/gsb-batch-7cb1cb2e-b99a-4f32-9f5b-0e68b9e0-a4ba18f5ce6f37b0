package protocol

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func samples() []struct {
	name  string
	frame Frame
} {
	return []struct {
		name  string
		frame Frame
	}{
		{"hello", Frame{Type: TypeHello, Version: 3, Text: "sim"}},
		{"open", Frame{Type: TypeOpen, Stream: 7, Source: "en", Target: "fr", Early: true}},
		{"audio", Frame{Type: TypeAudio, Stream: 7, Sequence: 65535, Packet: []byte{0xde, 0xad, 0xbe, 0xef}}},
		{"close", Frame{Type: TypeClose, Stream: 7}},
		{"speech", Frame{Type: TypeSpeech, Stream: 0x01020304, Packet: []byte{1, 2, 3}}},
		{"caption", Frame{Type: TypeCaption, Stream: 9, Kind: CaptionTranslation, Utterance: 3, Text: "voix à voix"}},
		{"partial", Frame{Type: TypeCaption, Stream: 9, Kind: CaptionSourcePartial, Utterance: 4, Text: "voix à"}},
		{"error", Frame{Type: TypeError, Stream: 9, Text: "unsupported: xx"}},
		{"closed", Frame{Type: TypeClosed, Stream: 9}},
		{"stats", Frame{Type: TypeStats, Text: `{"streams":1}`}},
	}
}

func TestFramesMatchSharedVectors(t *testing.T) {
	file, err := os.Open("../../../testdata/protocol_vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var vectors [][2]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, encoded, _ := strings.Cut(line, " ")
		vectors = append(vectors, [2]string{name, encoded})
	}

	want := samples()
	if len(vectors) != len(want) {
		t.Fatalf("%d vectors, %d samples", len(vectors), len(want))
	}
	for i, sample := range want {
		if vectors[i][0] != sample.name {
			t.Fatalf("vector %d is %q, want %q", i, vectors[i][0], sample.name)
		}
		encoded, err := sample.frame.Append(nil)
		if err != nil {
			t.Fatalf("%s: %v", sample.name, err)
		}
		if got := hex.EncodeToString(encoded); got != vectors[i][1] {
			t.Errorf("%s: encoded %s, want %s", sample.name, got, vectors[i][1])
		}
		raw, _ := hex.DecodeString(vectors[i][1])
		decoded, err := Read(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", sample.name, err)
		}
		if !reflect.DeepEqual(decoded, sample.frame) {
			t.Errorf("%s: decoded %+v, want %+v", sample.name, decoded, sample.frame)
		}
	}
}

func TestStreamRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	for _, sample := range samples() {
		if err := Write(&wire, sample.frame); err != nil {
			t.Fatal(err)
		}
	}
	for _, sample := range samples() {
		got, err := Read(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, sample.frame) {
			t.Errorf("got %+v, want %+v", got, sample.frame)
		}
	}
	if _, err := Read(&wire); !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF at end of stream, got %v", err)
	}
}

func TestMalformedFramesAreRejected(t *testing.T) {
	bodies := [][]byte{
		{},
		{0x7f, 0, 0, 0, 0},
		{byte(TypeOpen), 0, 0, 0, 1, 5, 'e'},
		{byte(TypeOpen), 0, 0, 0, 1, 1, 'e', 1, 'f'},
		{byte(TypeOpen), 0, 0, 0, 1, 1, 'e', 1, 'f', 2},
		{byte(TypeOpen), 0, 0, 0, 1, 1, 'e', 1, 'f', 0, 0},
		{byte(TypeCaption), 0, 0, 0, 1, 9, 0, 0, 0, 0},
		{byte(TypeCaption), 0, 0, 0, 1, 4, 0, 0, 0, 0},
		{byte(TypeCaption), 0, 0, 0, 1, 0},
		{byte(TypeError), 0, 0, 0, 1, 0xff, 0xfe},
		{byte(TypeAudio), 0, 0, 0, 1, 0},
		{byte(TypeHello), 0, 0, 0, 0, 1},
	}
	for _, body := range bodies {
		if _, err := Decode(body); err == nil {
			t.Errorf("Decode(%x) succeeded", body)
		}
	}

	if _, err := Read(bytes.NewReader([]byte{0, 1, 0, 1})); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized length: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 9, byte(TypeClose), 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body: %v", err)
	}
}

func TestOversizedFieldsFailToEncode(t *testing.T) {
	long := strings.Repeat("x", 256)
	if _, err := (Frame{Type: TypeOpen, Source: long, Target: "fr"}).Append(nil); err == nil {
		t.Error("long language accepted")
	}
	if _, err := (Frame{Type: TypeSpeech, Packet: make([]byte, MaxFrame)}).Append(nil); err == nil {
		t.Error("oversized packet accepted")
	}
	if _, err := (Frame{Type: 0x55}).Append(nil); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestCaptionKinds(t *testing.T) {
	cases := []struct {
		kind    CaptionKind
		name    string
		partial bool
	}{
		{CaptionSource, "source", false},
		{CaptionTranslation, "translation", false},
		{CaptionSourcePartial, "source", true},
		{CaptionTranslationPartial, "translation", true},
	}
	for _, c := range cases {
		if c.kind.String() != c.name || c.kind.Partial() != c.partial {
			t.Errorf("kind %d = %q partial %v", c.kind, c.kind.String(), c.kind.Partial())
		}
	}
}

func TestAppendPreservesPrefix(t *testing.T) {
	prefix := []byte("keep")
	out, err := Frame{Type: TypeClose, Stream: 1}.Append(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, prefix) || len(out) != len(prefix)+9 {
		t.Errorf("unexpected output %x", out)
	}
}
