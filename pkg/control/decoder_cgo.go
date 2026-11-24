//go:build cgo
// +build cgo

package control

import "github.com/Glimesh/waveguide/pkg/h264"

func init() {
	// Production path: use the real ffmpeg-backed H264 decoder.
	newImageDecoder = func() (imageDecoder, error) {
		return h264.NewH264Decoder()
	}
}
