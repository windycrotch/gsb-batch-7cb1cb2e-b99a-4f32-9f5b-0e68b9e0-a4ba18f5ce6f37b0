package rtspclient

import (
	"os"
	"testing"
)

// TestMain lets the test binary re-exec itself as a stand-in for ffmpeg.
func TestMain(m *testing.M) {
	maybeRunFakeFFmpeg()
	os.Exit(m.Run())
}
