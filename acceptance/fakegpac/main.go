// Command fakegpac is a local, deterministic stand-in for the gpac binary used
// by acceptance/E2E verification. Its behavior is controlled by environment
// variables:
//
//	FAKE_GPAC_MODE   success (default) | fail | slow
//	FAKE_GPAC_CALLS  optional file to append one "call" line per invocation
//	FAKE_GPAC_READY  optional file written once the slow process is running
//
// It understands just enough of the real command line: it finds the
// "-o <dir>/<id>/index.m3u8:segdur=..." argument and writes a playlist (and a
// segment) next to it. "slow" keeps running until killed, allowing deletion /
// cancellation / restart scenarios to be exercised.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func outputPath(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" {
			value := args[i+1]
			// Locate the option suffix; ":segdur=" is unambiguous and avoids
			// splitting on the Windows drive colon.
			if idx := strings.Index(value, ":segdur="); idx >= 0 {
				return value[:idx]
			}

			return value
		}
	}

	return ""
}

func main() {
	mode := os.Getenv("FAKE_GPAC_MODE")

	// A mode file takes precedence when present, allowing one long-lived
	// worker to switch fake behavior between scenarios without a restart.
	if modeFile := os.Getenv("FAKE_GPAC_MODE_FILE"); modeFile != "" {
		if data, err := os.ReadFile(modeFile); err == nil {
			if m := strings.TrimSpace(string(data)); m != "" {
				mode = m
			}
		}
	}

	if mode == "" {
		mode = "success"
	}

	if callLog := os.Getenv("FAKE_GPAC_CALLS"); callLog != "" {
		if f, err := os.OpenFile(callLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = f.WriteString("call\n")
			_ = f.Close()
		}
	}

	playlist := outputPath(os.Args[1:])
	writePlaylist := func(body string) {
		if playlist == "" {
			return
		}

		dir := filepath.Dir(playlist)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(playlist, []byte(body), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "seg0.ts"), []byte("fake-segment"), 0o644)
		}
	}

	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "fake gpac failure")
		os.Exit(7)
	case "slow":
		writePlaylist("#EXTM3U\n# fake gpac slow / long-running\n")

		if ready := os.Getenv("FAKE_GPAC_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte("ready"), 0o644)
		}
		// Block until the owning context/process terminates us. A sleeping
		// loop (rather than an empty select) avoids the runtime's
		// all-goroutines-asleep deadlock panic in a standalone binary.
		for {
			time.Sleep(time.Second)
		}
	default: // success
		writePlaylist("#EXTM3U\n# fake gpac success\n")
		os.Exit(0)
	}
}
