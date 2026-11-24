package flacenc

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Encoder encodes audio to FLAC format
type Encoder struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	BlockSize     int // samples per block (typically 4096)

	// mu makes repeated and concurrent use of one Encoder deterministic.
	// Per-encode statistics are reset at the beginning of Encode.
	mu sync.Mutex

	totalSamples uint64
	minBlockSize uint16
	maxBlockSize uint16
	minFrameSize uint32
	maxFrameSize uint32
	md5sum       [16]byte
	md5hash      []byte
}

// NewEncoder creates a new FLAC encoder
func NewEncoder(sampleRate, channels, bitsPerSample int) *Encoder {
	return &Encoder{
		SampleRate:    sampleRate,
		Channels:      channels,
		BitsPerSample: bitsPerSample,
		BlockSize:     4096,
		minBlockSize:  4096,
		maxBlockSize:  4096,
		minFrameSize:  0xFFFFFF,
		maxFrameSize:  0,
	}
}

// Encode encodes PCM samples to FLAC.
//
// Samples are interleaved and len(samples) must be a multiple of Channels.
// Encode resets all stream statistics, so an Encoder may be reused for
// another stream after this method returns.
func (e *Encoder) Encode(w io.Writer, samples []int32) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.Channels <= 0 || e.Channels > 8 {
		return fmt.Errorf("invalid channel count: %d", e.Channels)
	}
	if e.SampleRate <= 0 || e.SampleRate > 655350 {
		return fmt.Errorf("invalid sample rate: %d", e.SampleRate)
	}
	if e.BitsPerSample < 4 || e.BitsPerSample > 32 {
		return fmt.Errorf("invalid bits per sample: %d", e.BitsPerSample)
	}
	if len(samples)%e.Channels != 0 {
		return fmt.Errorf("sample count %d is not aligned to %d channels", len(samples), e.Channels)
	}

	blockSize := e.BlockSize
	if blockSize <= 0 {
		blockSize = 4096
	}
	if blockSize > 65535 {
		return fmt.Errorf("block size %d exceeds FLAC maximum 65535", blockSize)
	}

	// Every Encode starts a new stream. Do not retain total sample count,
	// frame-size extrema, block-size extrema, or MD5 state from a prior run.
	samplesPerChannel := len(samples) / e.Channels
	e.totalSamples = uint64(samplesPerChannel)
	e.minBlockSize = 0
	e.maxBlockSize = 0
	e.minFrameSize = ^uint32(0)
	e.maxFrameSize = 0
	e.md5sum = [16]byte{}
	e.md5hash = nil

	// We need to write frames to a buffer first because STREAMINFO precedes
	// them and is finalized with frame statistics.
	var buf bytes.Buffer

	// Compute MD5 of raw little-endian PCM bytes.
	md5h := md5.New()
	bytesPerSample := (e.BitsPerSample + 7) / 8
	var raw [4]byte
	for _, s := range samples {
		v := uint32(s)
		for i := 0; i < bytesPerSample; i++ {
			raw[i] = byte(v >> (8 * i))
		}
		md5h.Write(raw[:bytesPerSample])
	}
	sum := md5h.Sum(nil)
	copy(e.md5sum[:], sum)

	// Track the actual range of every emitted frame so STREAMINFO covers
	// short final frames as well as full blocks.
	frameNum := uint64(0)

	for offset := 0; offset < samplesPerChannel; offset += blockSize {
		frameBlockSize := blockSize
		if offset+frameBlockSize > samplesPerChannel {
			frameBlockSize = samplesPerChannel - offset
		}

		// Extract block samples (deinterleave).
		block := make([][]int32, e.Channels)
		for ch := 0; ch < e.Channels; ch++ {
			block[ch] = make([]int32, frameBlockSize)
			for i := 0; i < frameBlockSize; i++ {
				block[ch][i] = samples[(offset+i)*e.Channels+ch]
			}
		}

		frameSize, err := e.encodeFrame(&buf, block, frameNum)
		if err != nil {
			return fmt.Errorf("encode frame %d: %w", frameNum, err)
		}

		frameSize32 := uint32(frameSize)
		if frameSize32 < e.minFrameSize {
			e.minFrameSize = frameSize32
		}
		if frameSize32 > e.maxFrameSize {
			e.maxFrameSize = frameSize32
		}

		blockSize16 := uint16(frameBlockSize)
		if e.minBlockSize == 0 || blockSize16 < e.minBlockSize {
			e.minBlockSize = blockSize16
		}
		if blockSize16 > e.maxBlockSize {
			e.maxBlockSize = blockSize16
		}

		frameNum++
	}

	if samplesPerChannel == 0 {
		// There are no audio frames. Use the configured block size as a
		// conventional valid STREAMINFO value and mark frame sizes unknown.
		e.minBlockSize = uint16(blockSize)
		e.maxBlockSize = uint16(blockSize)
		e.minFrameSize = 0
		e.maxFrameSize = 0
	} else {
		if e.minFrameSize == ^uint32(0) {
			e.minFrameSize = 0
		}
	}

	// Now write everything with correct header.
	// Magic number.
	if _, err := w.Write([]byte("fLaC")); err != nil {
		return err
	}

	// STREAMINFO block.
	if err := e.writeStreamInfo(w); err != nil {
		return err
	}

	// Copy encoded frames.
	if _, err := io.Copy(w, &buf); err != nil {
		return err
	}

	return nil
}

// writeStreamInfo writes the STREAMINFO metadata block
func (e *Encoder) writeStreamInfo(w io.Writer) error {
	// Block header: 1 bit last-metadata-block flag + 7 bits type + 24 bits length
	// Type 0 = STREAMINFO, length = 34 bytes
	// 0x80 = last block flag set, type 0
	header := []byte{0x80, 0x00, 0x00, 0x22} // 0x22 = 34 in decimal
	if _, err := w.Write(header); err != nil {
		return err
	}

	// STREAMINFO data (34 bytes)
	var buf bytes.Buffer

	// The field values retain the exact observed range. Some widely used
	// parsers reject STREAMINFO block sizes below 16 even though the FLAC
	// format allows a short final frame; write the metadata lower bound in
	// that edge case while the frame header still states its true size.
	infoMinBlockSize := e.minBlockSize
	infoMaxBlockSize := e.maxBlockSize
	if infoMinBlockSize < 16 {
		infoMinBlockSize = 16
	}
	if infoMaxBlockSize < 16 {
		infoMaxBlockSize = 16
	}

	// Min block size (16 bits)
	binary.Write(&buf, binary.BigEndian, infoMinBlockSize)
	// Max block size (16 bits)
	binary.Write(&buf, binary.BigEndian, infoMaxBlockSize)
	// Min frame size (24 bits)
	buf.Write([]byte{byte(e.minFrameSize >> 16), byte(e.minFrameSize >> 8), byte(e.minFrameSize)})
	// Max frame size (24 bits)
	buf.Write([]byte{byte(e.maxFrameSize >> 16), byte(e.maxFrameSize >> 8), byte(e.maxFrameSize)})

	// Sample rate (20 bits) + channels-1 (3 bits) + bits per sample-1 (5 bits) + total samples (36 bits)
	// = 64 bits = 8 bytes
	sr := uint64(e.SampleRate)
	ch := uint64(e.Channels - 1)
	bps := uint64(e.BitsPerSample - 1)
	ts := e.totalSamples

	// Pack: SSSSSSSS SSSSSSSS SSSSCCCC CBBBBBTT TTTTTTTT TTTTTTTT TTTTTTTT TTTTTTTT
	packed := (sr << 44) | (ch << 41) | (bps << 36) | ts
	for i := 7; i >= 0; i-- {
		buf.WriteByte(byte(packed >> (i * 8)))
	}

	// MD5 signature (16 bytes)
	buf.Write(e.md5sum[:])

	_, err := w.Write(buf.Bytes())
	return err
}

// encodeFrame encodes a single frame
func (e *Encoder) encodeFrame(w io.Writer, block [][]int32, frameNum uint64) (int, error) {
	var buf bytes.Buffer
	bw := NewBitWriter(&buf)

	blockSize := len(block[0])

	// Frame header
	bw.ResetCRC8()
	bw.ResetCRC16()

	// Sync code (14 bits: 0x3FFE)
	bw.WriteBits(0x3FFE, 14)
	// Reserved (1 bit)
	bw.WriteBits(0, 1)
	// Blocking strategy (1 bit): 0 = fixed block size
	bw.WriteBits(0, 1)

	// Block size (4 bits) - encode block size
	blockSizeCode := getBlockSizeCode(blockSize)
	bw.WriteBits(uint64(blockSizeCode), 4)

	// Sample rate (4 bits)
	sampleRateCode := getSampleRateCode(e.SampleRate)
	bw.WriteBits(uint64(sampleRateCode), 4)

	// Channel assignment (4 bits)
	channelCode := uint64(e.Channels - 1)
	bw.WriteBits(channelCode, 4)

	// Sample size (3 bits)
	sampleSizeCode := getSampleSizeCode(e.BitsPerSample)
	bw.WriteBits(uint64(sampleSizeCode), 3)

	// Reserved (1 bit)
	bw.WriteBits(0, 1)

	// Frame/sample number (UTF-8 coded)
	bw.WriteUTF8(frameNum)

	// Block size (if code requires it)
	if blockSizeCode == 6 {
		bw.WriteBits(uint64(blockSize-1), 8)
	} else if blockSizeCode == 7 {
		bw.WriteBits(uint64(blockSize-1), 16)
	}

	// Sample rate (if code requires it)
	if sampleRateCode == 12 {
		bw.WriteBits(uint64(e.SampleRate/1000), 8)
	} else if sampleRateCode == 13 {
		bw.WriteBits(uint64(e.SampleRate), 16)
	} else if sampleRateCode == 14 {
		bw.WriteBits(uint64(e.SampleRate/10), 16)
	}

	// CRC-8 of frame header
	bw.Flush()
	crc8 := bw.GetCRC8()
	bw.WriteByte(crc8)

	// Subframes (one per channel)
	for ch := 0; ch < e.Channels; ch++ {
		if err := e.encodeSubframe(bw, block[ch]); err != nil {
			return 0, err
		}
	}

	// Byte-align
	bw.Flush()

	// CRC-16 of entire frame
	crc16 := bw.GetCRC16()
	bw.WriteUint16BE(crc16)

	// Write to output
	n, err := w.Write(buf.Bytes())
	return n, err
}

// encodeSubframe encodes a single subframe using FIXED prediction
func (e *Encoder) encodeSubframe(bw *BitWriter, samples []int32) error {
	if len(samples) < 16 {
		return e.encodeVerbatimSubframe(bw, samples)
	}

	// Try different prediction orders and pick the best. A predictor order
	// cannot exceed the block size because that would require more warmup
	// samples than the subframe contains.
	maxOrder := 4
	if len(samples) < maxOrder {
		maxOrder = len(samples)
	}
	bestOrder := 0
	bestSize := int64(1<<63 - 1)

	for order := 0; order <= maxOrder; order++ {
		residuals := computeFixedResiduals(samples, order)
		size := estimateRiceSize(residuals)
		if size < bestSize {
			bestSize = size
			bestOrder = order
		}
	}

	// Also try verbatim
	verbatimSize := int64(len(samples) * e.BitsPerSample)
	if verbatimSize < bestSize {
		return e.encodeVerbatimSubframe(bw, samples)
	}

	return e.encodeFixedSubframe(bw, samples, bestOrder)
}

// encodeVerbatimSubframe encodes without compression
func (e *Encoder) encodeVerbatimSubframe(bw *BitWriter, samples []int32) error {
	// Subframe header
	// 1 bit zero padding
	bw.WriteBits(0, 1)
	// 6 bits subframe type: 1 = VERBATIM
	bw.WriteBits(1, 6)
	// 1 bit wasted bits flag
	bw.WriteBits(0, 1)

	// Raw samples
	for _, s := range samples {
		bw.WriteBits(uint64(uint32(s)), e.BitsPerSample)
	}

	return nil
}

// encodeFixedSubframe encodes using FIXED prediction
func (e *Encoder) encodeFixedSubframe(bw *BitWriter, samples []int32, order int) error {
	// Subframe header
	// 1 bit zero padding
	bw.WriteBits(0, 1)
	// 6 bits subframe type: 001xxx = FIXED, xxx = order
	bw.WriteBits(uint64(0x08|order), 6)
	// 1 bit wasted bits flag
	bw.WriteBits(0, 1)

	// Warmup samples (unencoded)
	for i := 0; i < order; i++ {
		bw.WriteBits(uint64(uint32(samples[i])), e.BitsPerSample)
	}

	// Compute residuals
	residuals := computeFixedResiduals(samples, order)

	// Encode residuals with Rice coding
	return encodeRicePartition(bw, residuals)
}

// computeFixedResiduals computes residuals for FIXED prediction
func computeFixedResiduals(samples []int32, order int) []int32 {
	n := len(samples)
	residuals := make([]int32, n-order)

	switch order {
	case 0:
		// No prediction: residual = sample
		for i := order; i < n; i++ {
			residuals[i-order] = samples[i]
		}
	case 1:
		// residual = sample[i] - sample[i-1]
		for i := order; i < n; i++ {
			residuals[i-order] = samples[i] - samples[i-1]
		}
	case 2:
		// residual = sample[i] - 2*sample[i-1] + sample[i-2]
		for i := order; i < n; i++ {
			residuals[i-order] = samples[i] - 2*samples[i-1] + samples[i-2]
		}
	case 3:
		// residual = sample[i] - 3*sample[i-1] + 3*sample[i-2] - sample[i-3]
		for i := order; i < n; i++ {
			residuals[i-order] = samples[i] - 3*samples[i-1] + 3*samples[i-2] - samples[i-3]
		}
	case 4:
		// residual = sample[i] - 4*sample[i-1] + 6*sample[i-2] - 4*sample[i-3] + sample[i-4]
		for i := order; i < n; i++ {
			residuals[i-order] = samples[i] - 4*samples[i-1] + 6*samples[i-2] - 4*samples[i-3] + samples[i-4]
		}
	}

	return residuals
}

// estimateRiceSize estimates bits needed for Rice coding
func estimateRiceSize(residuals []int32) int64 {
	if len(residuals) == 0 {
		return 0
	}

	// Find optimal k parameter
	var sum int64
	for _, r := range residuals {
		if r >= 0 {
			sum += int64(r)
		} else {
			sum += int64(-r - 1)
		}
	}

	// Estimate k from average magnitude
	avg := float64(sum) / float64(len(residuals))
	k := 0
	for (1 << k) < int(avg) {
		k++
	}
	if k > 14 {
		k = 14
	}

	// Estimate total bits
	var bits int64
	for _, r := range residuals {
		var uval uint32
		if r >= 0 {
			uval = uint32(r) << 1
		} else {
			uval = (uint32(-r-1) << 1) | 1
		}
		q := uval >> k
		bits += int64(q) + 1 + int64(k)
	}

	return bits
}

// encodeRicePartition encodes residuals using Rice coding
func encodeRicePartition(bw *BitWriter, residuals []int32) error {
	if len(residuals) == 0 {
		// Write the partition header and a (never-used) Rice parameter.
		bw.WriteBits(0, 2) // encoding method
		bw.WriteBits(0, 4) // partition order
		bw.WriteBits(0, 4) // Rice parameter
		return nil
	}

	// Calculate optimal Rice parameter
	var sum int64
	for _, r := range residuals {
		if r >= 0 {
			sum += int64(r)
		} else {
			sum += int64(-r - 1)
		}
	}

	avg := float64(sum) / float64(len(residuals))
	k := 0
	for (1 << k) < int(avg) {
		k++
	}
	if k > 14 {
		k = 14
	}

	// Residual coding method (2 bits): 0 = Rice coding with 4-bit parameter
	bw.WriteBits(0, 2)
	// Partition order (4 bits): 0 = single partition
	bw.WriteBits(0, 4)
	// Rice parameter (4 bits for method 0, 5 bits for method 1)
	bw.WriteBits(uint64(k), 4)

	// Encode each residual
	for _, r := range residuals {
		if err := bw.WriteSignedRice(r, k); err != nil {
			return err
		}
	}

	return nil
}

// Helper functions for encoding
func getBlockSizeCode(blockSize int) int {
	switch blockSize {
	case 192:
		return 1
	case 576:
		return 2
	case 1152:
		return 3
	case 2304:
		return 4
	case 4608:
		return 5
	case 256:
		return 8
	case 512:
		return 9
	case 1024:
		return 10
	case 2048:
		return 11
	case 4096:
		return 12
	case 8192:
		return 13
	case 16384:
		return 14
	case 32768:
		return 15
	default:
		if blockSize <= 256 {
			return 6 // 8-bit block size - 1
		}
		return 7 // 16-bit block size - 1
	}
}

func getSampleRateCode(sampleRate int) int {
	switch sampleRate {
	case 88200:
		return 1
	case 176400:
		return 2
	case 192000:
		return 3
	case 8000:
		return 4
	case 16000:
		return 5
	case 22050:
		return 6
	case 24000:
		return 7
	case 32000:
		return 8
	case 44100:
		return 9
	case 48000:
		return 10
	case 96000:
		return 11
	default:
		if sampleRate%1000 == 0 && sampleRate/1000 <= 255 {
			return 12 // 8-bit kHz
		} else if sampleRate <= 65535 {
			return 13 // 16-bit Hz
		}
		return 14 // 16-bit tens of Hz
	}
}

func getSampleSizeCode(bitsPerSample int) int {
	switch bitsPerSample {
	case 8:
		return 1
	case 12:
		return 2
	case 16:
		return 4
	case 20:
		return 5
	case 24:
		return 6
	case 32:
		// There is no frame-header code for 32 bits; code 0 tells the
		// decoder to use STREAMINFO.
		return 0
	default:
		return 0 // Get from STREAMINFO
	}
}
