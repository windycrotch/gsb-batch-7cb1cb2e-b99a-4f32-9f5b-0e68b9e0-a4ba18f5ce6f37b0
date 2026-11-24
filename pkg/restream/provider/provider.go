package provider

import (
	"context"
	"time"
)

type Segment struct {
	URL       string
	Seq       uint64
	KeyMethod string
	KeyURL    string
	IV        string
	Duration  float64
}

type Provider interface {
	Get(ctx context.Context, bandwidth uint32) ([]Segment, time.Duration, error)
	// Closed reports whether the underlying playlist has ended (VOD playlist
	// carrying EXT-X-ENDLIST). A live playlist never closes.
	Closed() bool
	Info() string
}
