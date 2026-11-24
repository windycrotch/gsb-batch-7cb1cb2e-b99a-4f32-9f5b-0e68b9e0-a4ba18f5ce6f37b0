package pcm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	flac "github.com/tphakala/go-flac"
)

// TestDecoderResetSequenceMatchesFresh pins the pooling contract end to end: one
// decoder Reset through streams of different channel counts, bit depths and tag sets
// must yield, for each stream, exactly what a fresh NewDecoder on that stream yields
// (PCM bytes, Info, OutputBitDepth, Vendor and Tags), so no state from stream N can
// leak into stream N+1. The tagged -> untagged -> differently tagged order is what
// reddens if Reset fails to clear or re-derive the VORBIS_COMMENT fields on the
// success path; encodeStreaming gives every stream a real STREAMINFO MD5, so the
// per-stream MD5 verification has teeth as well.
func TestDecoderResetSequenceMatchesFresh(t *testing.T) {
	cfgs := []Config{
		{SampleRate: 44100, BitDepth: 16, Channels: 2, CompressionLevel: 5,
			Tags:   []flac.Tag{{Name: tagTitle, Value: "one"}, {Name: "ARTIST", Value: "alpha"}},
			Vendor: "vend-a"},
		{SampleRate: 48000, BitDepth: 24, Channels: 1, CompressionLevel: 8}, // no VORBIS_COMMENT at all
		{SampleRate: 32000, BitDepth: 8, Channels: 4, CompressionLevel: 3,
			Tags:   []flac.Tag{{Name: tagTitle, Value: "three"}},
			Vendor: "vend-c"},
	}
	streams := make([][]byte, len(cfgs))
	for i, cfg := range cfgs {
		streams[i] = encodeStreaming(t, cfg, genPCM(cfg, 5000+1000*i))
	}

	for _, seekable := range []bool{true, false} {
		t.Run(fmt.Sprintf("seekable=%v", seekable), func(t *testing.T) {
			d := new(Decoder) // zero value: the sync.Pool pattern
			for i := range cfgs {
				if err := d.Reset(sourceFor(streams[i], seekable)); err != nil {
					t.Fatalf("stream %d: Reset: %v", i, err)
				}
				fresh := mustDecoder(t, sourceFor(streams[i], seekable))

				if got, want := d.Info(), fresh.Info(); got != want {
					t.Fatalf("stream %d: Info() = %+v, fresh decoder reports %+v", i, got, want)
				}
				if got, want := d.OutputBitDepth(), fresh.OutputBitDepth(); got != want {
					t.Fatalf("stream %d: OutputBitDepth() = %d, fresh decoder reports %d", i, got, want)
				}
				if got, want := d.Vendor(), fresh.Vendor(); got != want {
					t.Fatalf("stream %d: Vendor() = %q, fresh decoder reports %q (stale vendor leaked)", i, got, want)
				}
				if got, want := d.Tags(), fresh.Tags(); !slices.Equal(got, want) {
					t.Fatalf("stream %d: Tags() = %v, fresh decoder reports %v (stale tags leaked)", i, got, want)
				}

				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("stream %d: decode after Reset: %v", i, err)
				}
				want, err := io.ReadAll(fresh)
				if err != nil {
					t.Fatalf("stream %d: fresh decode: %v", i, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("stream %d: PCM after Reset differs from fresh decode: %d vs %d bytes", i, len(got), len(want))
				}
			}
		})
	}
}

// TestDecoderFailedResetBlocksAllOperations pins the failed-Reset contract on every
// operation, not just Read: once Reset fails, no Read, WriteTo or SeekToSample may
// serve audio or metadata from the previous (fully decoded, still seekable) stream,
// and only another Reset revives the decoder. The previous stream is seekable on
// purpose: a stale seekable/rs pair would let SeekToSample resurrect the old stream.
func TestDecoderFailedResetBlocksAllOperations(t *testing.T) {
	good := makeStream(t, resetShapes["stereo16"], 9000)
	d := mustDecoder(t, bytes.NewReader(good))
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode good stream: %v", err)
	}

	rerr := d.Reset(bytes.NewReader([]byte("not a flac stream at all, just bytes")))
	if rerr == nil {
		t.Fatal("Reset onto garbage: want error, got nil")
	}
	if n, err := d.Read(make([]byte, 16)); n != 0 || !errors.Is(err, rerr) {
		t.Fatalf("Read after failed Reset = (%d, %v), want (0, the sticky Reset error)", n, err)
	}
	if n, err := d.WriteTo(io.Discard); n != 0 || !errors.Is(err, rerr) {
		t.Fatalf("WriteTo after failed Reset = (%d, %v), want (0, the sticky Reset error)", n, err)
	}
	// The previous stream was seekable; the failed Reset must have dropped that, so a
	// seek reports ErrSeekUnsupported rather than seeking the old stream.
	if _, err := d.SeekToSample(0); !errors.Is(err, flac.ErrSeekUnsupported) {
		t.Fatalf("SeekToSample after failed Reset = %v, want ErrSeekUnsupported (stale seek state leaked)", err)
	}
	if info := d.Info(); info != (flac.StreamInfo{}) {
		t.Fatalf("Info() after failed Reset = %+v, want zero", info)
	}

	// Only a successful Reset revives the decoder, and it then decodes the new stream
	// exactly as a fresh decoder would.
	if err := d.Reset(bytes.NewReader(good)); err != nil {
		t.Fatalf("recovery Reset: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("decode after recovery: %v", err)
	}
	if _, want := decodeAll(t, bytes.NewReader(good)); !bytes.Equal(got, want) {
		t.Fatalf("recovered decode differs from fresh: %d vs %d bytes", len(got), len(want))
	}
}

// TestSeekArgumentErrorsLeaveDecoderReadable pins that the argument and capability
// errors (ErrInvalidSeek for a negative index, ErrSeekUnsupported for a non-seekable
// source) touch no decoder state: an in-progress decode must continue byte-exactly
// after either, unlike a seek that fails part-way (which hard-fails the decoder).
func TestSeekArgumentErrorsLeaveDecoderReadable(t *testing.T) {
	data, _ := encodeRamp(t, 2, 16, 3*4096)
	_, want := decodeAll(t, bytes.NewReader(data))

	// Negative index on a seekable source.
	d := mustDecoder(t, bytes.NewReader(data))
	var got bytes.Buffer
	if _, err := io.CopyN(&got, d, 1000); err != nil {
		t.Fatalf("partial decode: %v", err)
	}
	if _, err := d.SeekToSample(-1); !errors.Is(err, flac.ErrInvalidSeek) {
		t.Fatalf("SeekToSample(-1) = %v, want ErrInvalidSeek", err)
	}
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("decode after ErrInvalidSeek: %v (the argument error disturbed the stream)", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decode after ErrInvalidSeek differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}

	// Any index on a non-seekable source.
	d2 := mustDecoder(t, readerOnly{bytes.NewReader(data)})
	got.Reset()
	if _, err := io.CopyN(&got, d2, 1000); err != nil {
		t.Fatalf("partial decode (non-seekable): %v", err)
	}
	if _, err := d2.SeekToSample(100); !errors.Is(err, flac.ErrSeekUnsupported) {
		t.Fatalf("SeekToSample on non-seekable source = %v, want ErrSeekUnsupported", err)
	}
	if _, err := io.Copy(&got, d2); err != nil {
		t.Fatalf("decode after ErrSeekUnsupported: %v (the capability error disturbed the stream)", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decode after ErrSeekUnsupported differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}
}

// TestSeekRecoveryAfterCutFrameTarget pins the recovery half of the cut-frame seek
// contract: a seek whose target lies in a frame the source cuts short fails sticky,
// but a later seek to an intact target on the same decoder clears the sticky error
// and decodes correctly, up to the cut frame where the truncation still reports.
func TestSeekRecoveryAfterCutFrameTarget(t *testing.T) {
	const block = 4096 // level 2 block size
	const samples = 3 * block
	full := encodeNoise(t, 2, 16, samples, true) // known total
	const frameBytes = 2 * 2                     // stereo 16-bit
	want, err := io.ReadAll(mustDecoder(t, bytes.NewReader(full)))
	if err != nil {
		t.Fatal(err)
	}
	lastStart := len(truncateAtFrameBoundary(t, full, 2)) // byte offset of the last frame
	cut := full[:lastStart+(len(full)-lastStart)/2]       // mid-body of the last frame

	dec := mustDecoder(t, bytes.NewReader(cut))
	// The target lies inside the cut last frame: the seek fails and sticks.
	if _, err := dec.SeekToSample(2*block + 100); !errors.Is(err, flac.ErrTruncatedStream) {
		t.Fatalf("SeekToSample into cut frame = %v, want ErrTruncatedStream", err)
	}
	if n, err := dec.Read(make([]byte, 8)); n != 0 || !errors.Is(err, flac.ErrTruncatedStream) {
		t.Fatalf("Read after the failed seek = (%d, %v), want (0, the sticky ErrTruncatedStream)", n, err)
	}

	// Re-seek to an intact target: the sticky state clears and decoding resumes.
	const target = block + 50
	landed, err := dec.SeekToSample(target)
	if err != nil || landed != target {
		t.Fatalf("recovery SeekToSample(%d) = (%d, %v), want (%d, nil)", target, landed, err, target)
	}
	got, rerr := io.ReadAll(dec)
	if !errors.Is(rerr, flac.ErrTruncatedStream) || !errors.Is(rerr, io.ErrUnexpectedEOF) {
		t.Fatalf("read after recovery: err = %v, want ErrTruncatedStream wrapping io.ErrUnexpectedEOF at the cut frame", rerr)
	}
	if wantRest := want[target*frameBytes : 2*block*frameBytes]; !bytes.Equal(got, wantRest) {
		t.Fatalf("read after recovery: got %d bytes, want the %d intact bytes up to the cut frame", len(got), len(wantRest))
	}
}

// TestDecoderResetOntoTruncatedStream pins that the reuse path does not swallow
// truncation: a decoder Reset onto a truncated stream reports ErrTruncatedStream
// from sequential reads, with exactly the bytes a fresh decoder yields before the
// cut. The first stream is decoded to completion so done/decoded carry stale values
// that a missing clear would turn into a silent clean EOF.
func TestDecoderResetOntoTruncatedStream(t *testing.T) {
	good := makeStream(t, resetShapes["stereo16"], 9000)
	full, _ := encodeRamp(t, 2, 16, 3*4096)
	cut := full[:len(full)-5] // chop into the last frame's body

	fresh := mustDecoder(t, bytes.NewReader(cut))
	want, wantErr := io.ReadAll(fresh)
	if !errors.Is(wantErr, flac.ErrTruncatedStream) {
		t.Fatalf("fresh decode of the cut stream: err = %v, want ErrTruncatedStream", wantErr)
	}

	d := mustDecoder(t, bytes.NewReader(good))
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode good stream: %v", err)
	}
	if err := d.Reset(bytes.NewReader(cut)); err != nil {
		t.Fatalf("Reset onto truncated stream: %v", err)
	}
	got, err := io.ReadAll(d)
	if !errors.Is(err, flac.ErrTruncatedStream) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("decode after Reset onto truncated stream: err = %v, want ErrTruncatedStream wrapping io.ErrUnexpectedEOF", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decode after Reset onto truncated stream: %d vs %d intact bytes", len(got), len(want))
	}
}

// TestDecoderResetRecoveryAppliesCurrentMD5Policy pins that MD5 policy is a property
// of the current Reset call, even on a decoder recovering from a failed Reset: the
// recovery Reset verifies by default (the poisoned interlude must not leave
// verification disabled), honors SkipMD5Verification for that stream, and drops the
// skip again on the next plain Reset.
func TestDecoderResetRecoveryAppliesCurrentMD5Policy(t *testing.T) {
	bad := corruptMD5(makeStream(t, resetShapes["mono24"], 9000))
	good := makeStream(t, resetShapes["stereo16"], 9000)

	d := mustDecoder(t, bytes.NewReader(good))
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode good stream: %v", err)
	}
	if err := d.Reset(bytes.NewReader([]byte("junk, not flac"))); err == nil {
		t.Fatal("Reset onto garbage: want error, got nil")
	}

	// Recovery with the default policy: the corrupted MD5 must be caught.
	if err := d.Reset(bytes.NewReader(bad)); err != nil {
		t.Fatalf("recovery Reset: %v", err)
	}
	if _, err := io.Copy(io.Discard, d); !errors.Is(err, flac.ErrMD5Mismatch) {
		t.Fatalf("decode after recovery Reset: err = %v, want ErrMD5Mismatch (verification must be on by default)", err)
	}

	// Recovery with SkipMD5Verification: the same stream decodes clean.
	if err := d.Reset(bytes.NewReader(bad), SkipMD5Verification()); err != nil {
		t.Fatalf("Reset(SkipMD5Verification): %v", err)
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode with SkipMD5Verification after recovery: %v", err)
	}

	// The skip does not stick: the next plain Reset verifies again.
	if err := d.Reset(bytes.NewReader(bad)); err != nil {
		t.Fatalf("Reset(default): %v", err)
	}
	if _, err := io.Copy(io.Discard, d); !errors.Is(err, flac.ErrMD5Mismatch) {
		t.Fatalf("decode after plain Reset: err = %v, want ErrMD5Mismatch (skip policy must not persist)", err)
	}
}
