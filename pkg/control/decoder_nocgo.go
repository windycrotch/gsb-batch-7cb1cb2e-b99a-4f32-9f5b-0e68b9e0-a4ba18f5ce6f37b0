//go:build !cgo
// +build !cgo

package control

import (
	"errors"
)

// errDecoderUnavailable is returned if code attempts to decode a thumbnail
// without a cgo build. In normal operation thumbnails are only decoded when a
// frame is available, and tests inject their own decoder via newImageDecoder.
var errDecoderUnavailable = errors.New("h264 image decoder is unavailable in this build (cgo disabled)")

func init() {
	newImageDecoder = func() (imageDecoder, error) {
		return nil, errDecoderUnavailable
	}
}
