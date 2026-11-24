//go:build cgo && portaudio
// +build cgo,portaudio

package sound

import (
	"io"

	"github.com/gordonklaus/portaudio"
	"github.com/pkg/errors"
	"github.com/youpy/go-wav"
)

// RecordWAV listens on the microphone and saves the signal to the given io.Writer
// it takes a stop channel to interrupt the recording
func RecordWAV(writer io.Writer, stopCh <-chan struct{}) error {
	samples := []wav.Sample{}
	channels := 1
	bitsPerSample := 32
	sampleRate := 44100

	portaudio.Initialize()
	defer portaudio.Terminate()

	in := make([]int32, 64)
	stream, err := portaudio.OpenDefaultStream(channels, 0, float64(sampleRate), len(in), in)
	if err != nil {
		return errors.Wrap(err, "error opening portaudio stream")
	}
	defer stream.Close()

	err = stream.Start()
	if err != nil {
		return errors.Wrap(err, "error starting stream")
	}

listen:
	for {
		err = stream.Read()
		if err != nil {
			return errors.Wrap(err, "error reading from stream")
		}

		for _, v := range in {
			samples = append(samples, wav.Sample{Values: [2]int{
				// Append the same value twice, worst case it will be used as stereo and averaged
				int(v), int(v),
			}})
		}

		select {
		case <-stopCh:
			break listen
		default:
		}
	}

	if err = stream.Stop(); err != nil {
		return errors.Wrap(err, "error stoping stream")
	}

	wr := wav.NewWriter(writer, uint32(len(samples)), uint16(channels), uint32(sampleRate), uint16(bitsPerSample))
	if err = wr.WriteSamples(samples); err != nil {
		return errors.Wrap(err, "error writing samples")
	}

	return nil
}
