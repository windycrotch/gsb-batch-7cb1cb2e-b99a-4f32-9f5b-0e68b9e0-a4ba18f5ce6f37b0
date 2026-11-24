package hls

import (
	"context"
	"fmt"
	"time"

	"github.com/grafov/m3u8"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
	"github.com/shaunschembri/restreamer/pkg/restream/request"
)

const mbDivider = 1048576

// activeKey tracks the encryption key currently in force while walking a
// playlist. An EXT-X-KEY tag applies to every following segment until the
// next key tag; the m3u8 parser only attaches the key to the segment
// immediately following the tag, so the provider carries the state forward
// itself and resolves every segment into a self-contained provider.Segment.
type activeKey struct {
	method string
	keyURL string
	iv     string
}

type Media struct {
	request      request.Request
	playlistURL  string
	lastMediaSeq uint64
	// closed is set once a fetched playlist carries EXT-X-ENDLIST.
	closed bool
	// initial distinguishes "no segment seen yet" from "last segment had
	// sequence number 0", otherwise a legitimate first segment with SeqId 0
	// would be filtered out by the SeqId <= lastMediaSeq comparison.
	initial bool
	// currentKey is the key inherited from the most recent EXT-X-KEY tag.
	// It is per-provider state: each Restream request owns its own provider
	// instance, so this never leaks between concurrent requests.
	currentKey *activeKey
}

func NewMedia(request request.Request) *Media {
	return &Media{
		request: request,
		initial: true,
	}
}

func (m Media) WithPlaylistURL(playlistURL string) *Media {
	m.playlistURL = playlistURL
	return &m
}

func (m Media) Info() string {
	return "Media"
}

// Closed reports whether the media playlist has ended.
func (m Media) Closed() bool {
	return m.closed
}

func (m *Media) Get(ctx context.Context, bandwidth uint32) ([]provider.Segment, time.Duration, error) {
	playlist, err := GetPlaylist(ctx, m.request, m.playlistURL)
	if err != nil {
		return nil, 0, err
	}

	mediaPlaylist, ok := playlist.playlist.(*m3u8.MediaPlaylist)
	if !ok {
		return nil, 0, fmt.Errorf("cannot assert to a media playlist from url %s", m.playlistURL)
	}
	m.closed = mediaPlaylist.Closed

	newSegmentsFound := false
	segments := make([]provider.Segment, 0)
	for _, mediaSegment := range mediaPlaylist.Segments {
		if mediaSegment == nil {
			continue
		}

		// Use the sequence number assigned by the playlist parser. The
		// Segments array keeps empty slots (nil entries), so deriving the
		// sequence from the loop index skips entries and loses segments.
		if !m.initial && mediaSegment.SeqId <= m.lastMediaSeq {
			continue
		}

		// A key tag attached to this segment changes the active key for it
		// and every following segment until the next key tag. A NONE method
		// clears the active key so later plain segments are written verbatim.
		if mediaSegment.Key != nil {
			switch mediaSegment.Key.Method {
			case "NONE", "":
				m.currentKey = nil
			default:
				key := &activeKey{method: mediaSegment.Key.Method, iv: mediaSegment.Key.IV}
				if mediaSegment.Key.URI != "" {
					keyURL, err := m.request.ResolveReference(mediaSegment.Key.URI, playlist.referenceURL)
					if err != nil {
						return nil, 0, fmt.Errorf("cannot resolve key URL: %w", err)
					}
					key.keyURL = keyURL.String()
				}
				m.currentKey = key
			}
		}

		newSegmentsFound = true
		m.initial = false
		m.lastMediaSeq = mediaSegment.SeqId

		url, err := m.request.ResolveReference(mediaSegment.URI, playlist.referenceURL)
		if err != nil {
			return nil, 0, fmt.Errorf("cannot resolve reference URL: %w", err)
		}

		segment := provider.Segment{
			URL:       url.String(),
			Seq:       mediaSegment.SeqId,
			KeyMethod: "NONE",
			Duration:  mediaSegment.Duration,
		}
		if m.currentKey != nil {
			segment.KeyMethod = m.currentKey.method
			segment.KeyURL = m.currentKey.keyURL
			segment.IV = m.currentKey.iv
		}

		segments = append(segments, segment)
	}

	// Reload playlist according to https://tools.ietf.org/html/draft-pantos-http-live-streaming-19#section-6.3.4
	reloadPlaylistAfter := time.Duration(mediaPlaylist.TargetDuration * float64(time.Second))
	if !newSegmentsFound {
		reloadPlaylistAfter /= 2
	}

	return segments, reloadPlaylistAfter, nil
}
