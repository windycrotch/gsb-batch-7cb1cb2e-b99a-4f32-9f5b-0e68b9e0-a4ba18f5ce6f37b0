package goff_test

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"testing"
	"unsafe"

	"github.com/itchio/goff"
	"github.com/itchio/goff/memfile"
	"github.com/stretchr/testify/assert"
)

func TestTranscode(t *testing.T) {
	assert := assert.New(t)
	logf := func(f string, args ...interface{}) {
		t.Logf(f, args...)
		// fmt.Printf("%s\n", fmt.Sprintf(f, args...))
	}

	loggedObjects := make(map[uintptr]string)

	goff.LogSetCallback(goff.LogLevel_DEBUG, func(ptr uintptr, level goff.LogLevel, line string, printPrefix bool) {
		if lo, ok := loggedObjects[ptr]; ok {
			logf("(%s) %s", lo, line)
		} else {
			logf("(unknown %x) %s", ptr, line)
		}
	})

	must := func(err error) {
		if err != nil {
			assert.NoError(err)
			t.FailNow()
		}
	}

	logf("Opening input...")

	inputPath := "testdata/sample.mp4"

	inputFormatContext := goff.FormatAllocContext()
	defer inputFormatContext.Free()
	loggedObjects[uintptr(unsafe.Pointer(inputFormatContext))] = "demuxer"

	// read the whole input into memory: this exercises the
	// Reader bridge (io.ReadSeeker -> AVIOContext)
	inputData, err := ioutil.ReadFile(inputPath)
	must(err)

	inputReader := goff.NewReader(bytes.NewReader(inputData), int64(len(inputData)))
	defer inputReader.Free()

	err = inputFormatContext.OpenReader(inputReader, nil, nil)
	must(err)

	assert.EqualValues(2, inputFormatContext.NbStreams())

	inputVideoStream := inputFormatContext.Streams()[0]
	assert.EqualValues("video", inputVideoStream.CodecParameters().CodecType().String())

	inputFormatContext.DumpFormat(0, inputPath, false)

	numFrames := 0

	inputVideoDecoder := inputVideoStream.CodecParameters().CodecID().FindDecoder()
	assert.NotNil(inputVideoDecoder)

	inputVideoDecoderContext := inputVideoDecoder.AllocContext3()
	assert.NotNil(inputVideoDecoderContext)
	loggedObjects[uintptr(unsafe.Pointer(inputVideoDecoderContext))] = "decoder"

	inputVideoStream.CodecParameters().ToContext(inputVideoDecoderContext)

	err = inputVideoDecoderContext.Open2(inputVideoDecoder, nil)
	must(err)

	logf("Opening output...")

	memBuffer := memfile.New()

	outputFormat := goff.GuessFormat("mp4", "", "")
	if outputFormat == nil {
		assert.NotNil(outputFormat)
		t.FailNow()
	}
	logf("Guessed format: %s", outputFormat.LongName())

	outputFormatContext, err := goff.FormatAllocOutputContext2(outputFormat, "", "")
	must(err)
	defer outputFormatContext.Free()
	loggedObjects[uintptr(unsafe.Pointer(outputFormatContext))] = "muxer"

	outputWriter := goff.NewWriter(memBuffer)
	defer outputWriter.Free()
	outputFormatContext.SetWriter(outputWriter)

	outputVideoStream := outputFormatContext.NewStream(inputVideoDecoder)
	outputVideoStream.SetID(0)

	outputCodecID := goff.CodecID_H264
	outputVideoEncoder := outputCodecID.FindEncoder()
	assert.NotNil(outputVideoEncoder)

	outputVideoEncoderContext := outputVideoEncoder.AllocContext3()
	assert.NotNil(outputVideoEncoderContext)
	loggedObjects[uintptr(unsafe.Pointer(outputVideoEncoderContext))] = "encoder"

	outputVideoEncoderContext.SetCodecType(goff.MediaType_Video)
	outputVideoEncoderContext.SetCodecID(goff.CodecID_H264)
	outputVideoEncoderContext.SetPixelFormat(goff.PixelFormat_YUV420P)

	outputVideoStream.SetTimeBase(goff.TIME_BASE_Q)
	outputVideoEncoderContext.SetTimeBase(goff.NewRational(1, 120))
	outputVideoEncoderContext.SetWidth(inputVideoDecoderContext.Width())
	outputVideoEncoderContext.SetHeight(inputVideoDecoderContext.Height())

	goff.OptSet(outputVideoEncoderContext.PrivData(), "preset", "ultrafast", goff.SearchFlags_CHILDREN)

	err = outputVideoEncoderContext.Open2(outputVideoEncoder, nil)
	must(err)

	err = outputVideoStream.CodecParameters().FromContext(outputVideoEncoderContext)
	must(err)

	// outputFormatContext.DumpFormat(0, outPath, true)

	must(outputFormatContext.WriteHeader(nil))

	inputFrame := goff.FrameAlloc()
	assert.NotNil(inputFrame)
	defer inputFrame.Free()

	var packet goff.Packet
	defer packet.Unref()

	writeEncodedPackets := func(last bool) {
		for {
			var outPacket goff.Packet
			outPacket.Init()

			err := outputVideoEncoderContext.ReceivePacket(&outPacket)
			if err != nil {
				if goff.IsEOF(err) {
					// all flushed!
					return
				}
				if !last && goff.IsEAGAIN(err) {
					// will get more packets later
					return
				}
				must(err)
			}

			outPacket.SetStreamIndex(outputVideoStream.Index())
			outPacket.RescaleTs(outputVideoEncoderContext.TimeBase(), outputVideoStream.TimeBase())

			err = outputFormatContext.InterleavedWriteFrame(&outPacket)
			must(err)
		}
	}

	receiveDecodedFrames := func() {
		for {
			err = inputVideoDecoderContext.ReceiveFrame(inputFrame)
			if err != nil {
				if goff.IsEAGAIN(err) || goff.IsEOF(err) {
					return
				}
				must(err)
			}
			numFrames++

			scaledPts := inputFrame.Pts().Rescale(inputVideoDecoderContext.TimeBase(), outputVideoEncoderContext.TimeBase())
			if scaledPts.IsNop() {
				scaledPts = 0
			}
			inputFrame.SetPts(scaledPts)

			err = outputVideoEncoderContext.SendFrame(inputFrame)
			must(err)

			writeEncodedPackets(false)
		}
	}

	for {
		packet.Init()

		err = inputFormatContext.ReadFrame(&packet)
		if err != nil {
			if goff.IsEOF(err) {
				break
			}
			must(err)
		}

		if packet.StreamIndex() != inputVideoStream.Index() {
			// ignore audio packets
			continue
		}

		packet.RescaleTs(inputVideoStream.TimeBase(), inputVideoDecoderContext.TimeBase())

		err = inputVideoDecoderContext.SendPacket(&packet)
		if err != nil {
			must(err)
		}

		receiveDecodedFrames()
	}
	receiveDecodedFrames()

	flushEncoder := func() {
		err := outputVideoEncoderContext.SendFrame(nil)
		must(err)

		writeEncodedPackets(true)
	}

	flushEncoder()

	logf("Processed %d inputFrames in total", numFrames)
	assert.EqualValues(23, numFrames)

	err = outputFormatContext.WriteTrailer()
	must(err)

	writerLen, err := memBuffer.Seek(0, io.SeekEnd)
	must(err)
	assert.True(writerLen > 0, "transcoded output must not be empty")
	logf("Transcoded output is (%d KB) in memory", writerLen/1024)

	// read the transcoded data back from memory: it must be a valid
	// mp4 with exactly one (video) stream — this validates that the
	// Writer bridge and memfile seek/write offsets produced a
	// well-formed file
	_, err = memBuffer.Seek(0, io.SeekStart)
	must(err)
	outputData := make([]byte, writerLen)
	_, err = io.ReadFull(memBuffer, outputData)
	must(err)

	verifyContext := goff.FormatAllocContext()
	defer verifyContext.Free()

	verifyReader := goff.NewReader(bytes.NewReader(outputData), writerLen)
	defer verifyReader.Free()

	err = verifyContext.OpenReader(verifyReader, nil, nil)
	must(err)

	err = verifyContext.FindStreamInfo(nil)
	must(err)

	assert.EqualValues(1, verifyContext.NbStreams())
	verifyStream := verifyContext.Streams()[0]
	assert.EqualValues("video", verifyStream.CodecParameters().CodecType().String())

	outPath := "out.mp4"
	outFile, err := os.Create(outPath)
	must(err)
	defer outFile.Close()

	_, err = outFile.Write(outputData)
	must(err)
}
