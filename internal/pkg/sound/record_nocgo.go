//go:build !cgo || !portaudio
// +build !cgo !portaudio

package sound

import (
	"io"

	"github.com/pkg/errors"
)

// RecordWAV is only available when the binary is built with cgo enabled
// (it relies on the portaudio native library). When cgo is disabled it
// returns an error instead of pulling a native dependency into the build.
func RecordWAV(writer io.Writer, stopCh <-chan struct{}) error {
	return errors.New("recording is not available: musig was built without cgo/portaudio support")
}
