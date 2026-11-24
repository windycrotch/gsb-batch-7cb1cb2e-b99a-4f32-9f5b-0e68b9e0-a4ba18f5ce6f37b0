package restream

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
)

const (
	defaultUserAgent      = "restreamer"
	defaultBandwidth      = 10485760
	defaultReadBufferSize = 1048576
	mbDivider             = 1048576
)

type Restream struct {
	UserAgent       string
	MaxBandwidth    uint32
	Writer          io.Writer
	SegmentProvider provider.Provider
	ReadBufferSize  int
	// streamedBytes and currentBandwidth are shared between Start's loop
	// and the getSegments goroutine. They are only ever touched through the
	// sync/atomic helpers (or their own atomic methods).
	streamedBytes    atomic.Int64
	currentBandwidth atomic.Uint32
	segments         chan provider.Segment
	errors           chan error
}

func (r *Restream) init(ctx context.Context, playlistURL string) error {
	r.segments = make(chan provider.Segment, 1024)
	r.errors = make(chan error, 1024)

	if r.MaxBandwidth == 0 {
		r.MaxBandwidth = defaultBandwidth
	}
	r.currentBandwidth.Store(r.MaxBandwidth)

	if r.ReadBufferSize == 0 {
		r.ReadBufferSize = defaultReadBufferSize
	}

	if r.UserAgent == "" {
		r.UserAgent = defaultUserAgent
	}

	if r.SegmentProvider == nil {
		segmentProvider, err := r.detectStream(ctx, playlistURL, r.UserAgent, r.MaxBandwidth)
		if err != nil {
			return err
		}

		r.SegmentProvider = segmentProvider
	}

	return nil
}
