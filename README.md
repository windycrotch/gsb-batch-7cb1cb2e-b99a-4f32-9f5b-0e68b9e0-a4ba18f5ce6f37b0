# Goffmpeg
[![Build & Test](https://github.com/xfrr/goffmpeg/actions/workflows/build_and_test.yml/badge.svg)](https://github.com/xfrr/goffmpeg/actions/workflows/build_and_test.yml)
[![Codacy Badge](https://api.codacy.com/project/badge/Grade/93e018e5008b4439acbb30d715b22e7f)](https://www.codacy.com/app/francisco.romero/goffmpeg?utm_source=github.com&amp;utm_medium=referral&amp;utm_content=xfrr/goffmpeg&amp;utm_campaign=Badge_Grade)
[![Go Report Card](https://goreportcard.com/badge/github.com/xfrr/goffmpeg)](https://goreportcard.com/report/github.com/xfrr/goffmpeg)
[![GoDoc](https://godoc.org/github.com/xfrr/goffmpeg?status.svg)](https://godoc.org/github.com/xfrr/goffmpeg)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

FFMPEG wrapper written in GO

## Features

- [x] Transcoding
- [x] Streaming
- [x] Progress
- [x] Stopping a running transcode
- [x] Filters
- [x] Thumbnails
- [x] Watermark
- [ ] Concatenation
- [ ] Subtitles

## Stopping and progress

`Run` starts ffmpeg and returns a channel that reports the final error. The
progress channel returned by `Output` is closed exactly once, either when the
transcode finishes on its own or when `Stop` is called:

```go
trans := new(transcoder.Transcoder)
if err := trans.Initialize("input.mp4", "output.mp4"); err != nil {
    log.Fatal(err)
}

done := trans.Run(true)
progress := trans.Output()

go func() {
    for p := range progress {
        fmt.Printf("%s (%0.1f%%)\n", p.CurrentTime, p.Progress)
    }
}()

// Later, from another goroutine:
if err := trans.Stop(); err != nil {
    log.Printf("stop failed: %v", err)
}
// Ranging over `progress` now completes promptly instead of blocking.
```

`Stop` never blocks indefinitely: it asks ffmpeg to quit, waits a short grace
period and kills the process if needed. Calling it when the transcode has
already finished returns `nil` immediately. Input and output pipes created with
`CreateInputPipe`/`CreateOutputPipe` are closed when the run ends, whether it
was stopped or completed.

For live streams (a total duration of `0`) the reported `Progress` percentage
stays at `0`; `CurrentTime`, `CurrentBitrate` and `FramesProcessed` are still
reported.

## Dependencies
- [FFmpeg](https://www.ffmpeg.org/)
- [FFProbe](https://www.ffmpeg.org/ffprobe.html)

## Supported platforms

 - Linux
 - OS X
 - Windows

## Installation
Install the package with the following command:
```shell
go get github.com/xfrr/goffmpeg
```

## Usage
Check the [examples](./examples)
