package restream

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/grafov/m3u8"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
	"github.com/shaunschembri/restreamer/pkg/restream/provider/hls"
	"github.com/shaunschembri/restreamer/pkg/restream/request"
)

// Start runs the restream pipeline. It has a pointer receiver on purpose:
// the atomic counters and channels must not be copied between the Start loop
// and the getSegments goroutine, otherwise the race detector (and correct
// synchronization) breaks down.
func (r *Restream) Start(ctx context.Context, playlistURL string) error {
	if err := r.init(ctx, playlistURL); err != nil {
		return err
	}

	segmentsContext, cancel := context.WithCancel(ctx)

	// Wait for the pull/decrypt/write goroutine to stop before returning so
	// that it can never touch the channels or writer after Start has handed
	// control back to the caller.
	var goroutineDone sync.WaitGroup
	goroutineDone.Add(1)
	go func() {
		defer goroutineDone.Done()
		r.getSegments(segmentsContext)
	}()
	defer func() {
		cancel()
		goroutineDone.Wait()
	}()

	for {
		segments, sleepTime, err := r.SegmentProvider.Get(ctx, r.currentBandwidth.Load())
		if err != nil {
			// A cancelled context is a clean shutdown even if the playlist
			// request also returned an error.
			if ctx.Err() != nil {
				r.displayStats()
				return nil
			}
			return fmt.Errorf("failed to get new segments: %w", err)
		}

		for _, segment := range segments {
			select {
			case <-ctx.Done():
				// Stop handing new segments to the writer goroutine: once the
				// context is done no further segment requests may be issued.
				r.displayStats()
				return nil
			case r.segments <- segment:
			}
		}

		// A VOD playlist has ended. Close the segment queue instead of
		// cancelling the context so the writer goroutine drains every
		// segment already enqueued, then wait for it to finish cleanly.
		if r.SegmentProvider.Closed() {
			close(r.segments)
			goroutineDone.Wait()
			r.displayStats()
			select {
			case err := <-r.errors:
				return err
			default:
				return nil
			}
		}

		select {
		case <-ctx.Done():
			r.displayStats()
			return nil
		case err := <-r.errors:
			if ctx.Err() != nil {
				r.displayStats()
				return nil
			}
			return err
		case <-time.After(sleepTime):
			r.displayStats()
		}
	}
}

func (r *Restream) displayStats() {
	statsString := fmt.Sprintf("Streamed: %5.1fMB | Calculated Bandwidth: %4.1fMb/s",
		float64(r.streamedBytes.Load())/mbDivider,
		float64(r.currentBandwidth.Load())/mbDivider)

	log.Printf("%s | Playlist Type: %s", statsString, r.SegmentProvider.Info())
}

func (r *Restream) detectStream(ctx context.Context, playlistURL, userAgent string, maxBandwidth uint32) (provider.Provider, error) {
	request := request.New(userAgent)
	playlist, err := hls.GetPlaylist(ctx, request, playlistURL)
	if err != nil {
		return nil, fmt.Errorf("cannot get playlist: %w", err)
	}

	switch playlist.Type() {
	case m3u8.MEDIA:
		return hls.NewMedia(request).WithPlaylistURL(playlistURL), nil
	case m3u8.MASTER:
		return hls.NewMaster(request, maxBandwidth).WithPlaylist(playlist), nil
	default:
		return nil, fmt.Errorf("invalid playlist list type found at %s", playlistURL)
	}
}
