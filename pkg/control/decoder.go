package control

import "image"

// imageDecoder turns an encoded video frame into a decoded image. It mirrors
// the small surface of h264.H264Decoder that the control plane actually uses.
type imageDecoder interface {
	Decode(nalu []byte) (image.Image, error)
	Close()
}

// newImageDecoder builds the decoder used for thumbnail frames. The concrete
// implementation lives behind a build-tagged file: decoder_cgo.go wires it to
// the real ffmpeg-backed h264.H264Decoder when cgo is available, while
// decoder_nocgo.go provides an overridable stub otherwise. Tests replace this
// variable with an in-memory fake so the control teardown logic can be
// exercised without the native ffmpeg toolchain.
var newImageDecoder func() (imageDecoder, error)
