package config

import (
	"github.com/OmGuptaIND/display"
)

const RECORDING_DIR = "recordings"

var DEFAULT_DISPLAY_OPTS = display.DisplayOptions{
	Width:  1280,
	Height: 720,
	Depth:  24,
}

// ChunkInfo represents the information of a chunk, to be used by the Watcher.
type ChunkInfo struct {
	RecorderID string
	ChunkName  string
	ChunkPath  string
	ChunkSize  int64
}
