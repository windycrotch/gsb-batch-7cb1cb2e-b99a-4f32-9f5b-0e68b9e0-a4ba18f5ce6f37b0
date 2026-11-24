package restreamer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/viper"

	"github.com/shaunschembri/restreamer/pkg/restream"
)

const mbMultiplier = 1048576

// ErrStreamNotFound is returned when the requested stream id is not present
// in the configuration. Callers (for example the HTTP server) use it to
// distinguish a missing stream from an upstream pull failure.
var ErrStreamNotFound = errors.New("stream not found")

func start(ctx context.Context, writer io.Writer, streamID string) error {
	streamer := restream.Restream{
		Writer:         writer,
		MaxBandwidth:   uint32(viper.GetFloat64("max-bandwidth") * mbMultiplier),
		ReadBufferSize: int(viper.GetFloat64("read-buffer") * mbMultiplier),
	}

	streams := viper.GetStringMapString("streams")
	streamURL, ok := streams[streamID]
	if !ok {
		return fmt.Errorf("%w: url for stream with id %s not found in config", ErrStreamNotFound, streamID)
	}

	if err := streamer.Start(ctx, streamURL); err != nil {
		return fmt.Errorf("restreamer error %w", err)
	}

	return nil
}

// statusRecorder captures the first non-200 status set before the body is
// written. It lets the handler report a meaningful error status even though
// the stream writer starts flushing bytes as soon as segments arrive.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer when it supports flushing so that
// live streaming behaviour is preserved.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
