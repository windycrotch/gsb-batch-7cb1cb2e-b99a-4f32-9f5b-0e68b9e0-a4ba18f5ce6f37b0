package util

import (
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ConcatVideos concats multiples videos into one using ffmpeg
//
// Read this to get a basic understanding of what's going on here. It's short and easy to understand:
// https://ffmpeg.org/ffmpeg-filters.html#Filtering-Introduction
//
// Used filters and such:
//   - concat: concat video/audio streams, see https://ffmpeg.org/ffmpeg-filters.html#concat
//   - scale: resize videos to desired size, adding black padding and centering it, see https://ffmpeg.org/ffmpeg-filters.html#scale-1
//     and https://stackoverflow.com/a/48853654/1440785
//   - anullsrc: append silent audio for videos without sound, see https://ffmpeg.org/ffmpeg-filters.html#anullsrc
//     and https://stackoverflow.com/a/46058429/1440785
//
// Full example:
//
//	 ffmpeg -y \
//	  -i video1.mp4 \
//	  -i video2.mkv \
//	  -f lavfi -t 0.1 -i anullsrc \
//	-filter_complex "
//	  [0:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,setsar=1[v0];
//	  [1:v]scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,setsar=1[v1];
//	  [v0][0:a][v1][2:a]concat=n=2:v=1:a=1[v][a]
//	" \
//	-map "[v]" -map "[a]" \
//	-metadata comment="Created with submonkey ..." \
//
// output.mp4
func ConcatVideos(infiles []string, scale, comment, outfile string) ([]byte, error) {
	if len(infiles) == 0 {
		return nil, errors.New("no input videos to concatenate")
	}
	var filter, concat string
	for i, filename := range infiles {
		// ";" separates filterchains; avoid literal newlines in the argument so
		// Windows .cmd/.bat tool shims do not mis-parse the command line.
		filter += fmt.Sprintf("[%d:v]scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2,setsar=1[v%d];", i, scale, scale, i)
		meta, err := VideoMetadata(filename)
		if err != nil {
			return nil, err
		}
		if meta.HasAudio {
			concat += fmt.Sprintf("[v%d][%d:a]", i, i)
		} else {
			concat += fmt.Sprintf("[v%d][%d:a]", i, len(infiles)) // anullsrc (silence audio stream)
		}
	}
	filter += fmt.Sprintf("%sconcat=n=%d:v=1:a=1[v][a]", concat, len(infiles))

	// All-or-nothing output: ffmpeg writes to a temporary file in the same
	// directory and the result is atomically renamed over outfile only after
	// success. A failed concat therefore never leaves a truncated outfile and
	// never overwrites a pre-existing user file.
	tmpOut, err := createTempOutput(outfile)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpOut)
		}
	}()

	args := make([]string, 0)
	args = append(args, "-y")
	for _, filename := range infiles {
		args = append(args, "-i", filename)
	}
	args = append(args, "-f", "lavfi", "-t", "0.1", "-i", "anullsrc")
	args = append(args, "-filter_complex", filter)
	args = append(args, "-metadata", fmt.Sprintf("comment=%s", comment))
	args = append(args, "-map", "[v]", "-map", "[a]", tmpOut)
	cmd := exec.Command("ffmpeg", args...)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return output, runErr
	}
	if info, err := os.Stat(tmpOut); err != nil || info.Size() == 0 {
		if err == nil {
			return output, fmt.Errorf("ffmpeg reported success but output %s is empty", tmpOut)
		}
		return output, fmt.Errorf("ffmpeg reported success but output is missing: %w", err)
	}
	if err := commitOutput(tmpOut, outfile); err != nil {
		return output, err
	}
	committed = true
	return output, nil
}

// createTempOutput creates a closed zero-byte temporary file in the output
// directory with a video-friendly extension. The caller must remove the
// returned path unless commitOutput succeeds.
func createTempOutput(outfile string) (string, error) {
	dir := filepath.Dir(outfile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Base(outfile)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = "output"
	}
	pattern := stem + ".tmp-*" + ext
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// commitOutput atomically replaces outfile with tmpOut. os.Rename performs an
// atomic replace on both Unix and modern Windows/Go; retries cover transient
// Windows locks (antivirus/indexer) on a freshly written file.
func commitOutput(tmpOut, outfile string) error {
	var renameErr error
	for attempt := 0; attempt < 6; attempt++ {
		if renameErr = os.Rename(tmpOut, outfile); renameErr == nil {
			return nil
		}
		time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
	}
	// Defensive fallback for platforms/filesystems with replacement quirks.
	if err := copyFile(tmpOut, outfile); err != nil {
		return renameErr
	}
	return os.Remove(tmpOut)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

type AVMetadata struct {
	Duration time.Duration
	HasAudio bool
}

// VideoMetadata reads metadata from the given filename
func VideoMetadata(filename string) (*AVMetadata, error) {
	// { "streams": [
	//    { "codec_type": "video", "duration": "63.766992", ... },
	//    ...
	// ] }
	cmd := exec.Command("ffprobe", "-show_streams", "-print_format", "json", "-loglevel", "error", filename)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	res := gjson.GetBytes(output, `streams.0.duration`)
	if !res.Exists() {
		return nil, errors.New("unexpected ffprobe output, cannot find duration")
	}
	duration, err := strconv.ParseFloat(res.String(), 64)
	if err != nil {
		return nil, err
	}
	res = gjson.GetBytes(output, `streams.#(codec_type="audio")`)
	hasAudio := res.Exists()
	return &AVMetadata{
		Duration: time.Second * time.Duration(duration),
		HasAudio: hasAudio,
	}, nil
}
