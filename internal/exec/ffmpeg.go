// Package mediaexec wraps external command execution helpers.
package mediaexec

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steipete/camsnap/internal/redact"
)

const ffmpegLogTailLines = 20

// RunFFmpeg executes ffmpeg with a timeout, returning combined output on error.
func RunFFmpeg(ctx context.Context, args ...string) error {
	_, err := RunFFmpegWithOutput(ctx, args...)
	return err
}

// RunFFmpegWithOutput returns combined stdout/stderr output alongside error.
func RunFFmpegWithOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	safeOutput := redactRTSPCredentials(string(output))
	if err != nil {
		safeArgs := make([]string, len(args))
		for i, arg := range args {
			safeArgs[i] = redactRTSPCredentials(arg)
		}
		return safeOutput, fmt.Errorf("ffmpeg %v failed: %w\n%s", safeArgs, err, safeOutput)
	}
	return safeOutput, nil
}

// RunFFmpegWithStderrLines streams ffmpeg stderr to a callback. It returns a
// short log tail, an ffmpeg exit error, and any process setup or log read error.
func RunFFmpegWithStderrLines(ctx context.Context, args []string, onLine func(string)) (string, error, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	logTail := make([]string, 0, ffmpegLogTailLines)
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		logTail = append(logTail, line)
		if len(logTail) > ffmpegLogTailLines {
			logTail = logTail[1:]
		}
		if onLine != nil {
			onLine(line)
		}
	}
	if err := scanner.Err(); err != nil {
		// No reader remains to drain stderr; stop the writer before waiting.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return strings.Join(logTail, "\n"), nil, fmt.Errorf("read ffmpeg logs: %w", err)
	}
	return strings.Join(logTail, "\n"), cmd.Wait(), nil
}

// redactRTSPCredentials hides userinfo in any RTSP/RTSPS URLs embedded in
// ffmpeg output. It is a thin wrapper around the shared redact package so
// log handling and the gortsplib capture path apply identical rules.
func redactRTSPCredentials(text string) string {
	return redact.Text(text)
}

// WithTimeout returns a derived context that times out.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}

// HasBinary reports whether a named executable is available in PATH.
func HasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ClassifyError inspects ffmpeg stderr to differentiate common failures.
func ClassifyError(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "permission denied") || strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "failed to create avcapturedeviceinput") ||
		strings.Contains(lower, "not authorized to capture video"):
		return "permission"
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "auth"):
		return "auth"
	case strings.Contains(lower, "connection refused"):
		return "network-refused"
	case strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout"):
		return "network-timeout"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "no such file or directory") || strings.Contains(lower, "could not find video device"):
		return "not-found"
	default:
		return "unknown"
	}
}
