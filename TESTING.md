# Testing / verification notes

## Deterministic end-to-end runs without Reddit

`CreateVideo` fetches posts from Reddit by default. For offline/deterministic
acceptance runs, set `SUBMONKEY_POSTS_FILE` to a JSON fixture:

```json
[
  {"id": "aaa", "title": "A", "url": "https://example.test/aaa", "permalink": "/r/x/aaa"},
  {"id": "bbb", "title": "B", "url": "https://example.test/bbb", "permalink": "/r/x/bbb"}
]
```

or `{"posts": [ ... ]}`. The real CLI entry point can then be driven with
fake `youtube-dl`, `ffprobe` and `ffmpeg` executables placed first on `PATH`.

## Cache lifecycle contract

- Committed clips live at `<cache-dir>/<post-id>.mp4` and are only published
  atomically after download + `ffprobe` validation.
- In-flight downloads use private `.sm-dl-*` staging directories; failures,
  duration rejection and cleanup remove them immediately. A hard-killed run
  leaves a locked staging/run directory whose OS lock is released by the
  kernel; the next generation reaps it and never treats its contents as
  committed clips.
- Each generation owns a `.sm-run-*` directory with `claim.json`, a liveness
  `run.lock` and private hard-linked `pins/` copies of concatenated clips.
  Cleanup only removes entries older than `--cache-keep` that no live claim
  protects; a cross-process lock serializes claims and cleanup.
- Existing `<id>.mp4` cache hits are reused only when they stat as non-empty
  files and pass `ffprobe` (including the per-clip duration limit).

## Output contract

`util.ConcatVideos` writes to a temporary file next to the destination and
atomically renames it over the destination only after `ffmpeg` exits
successfully and the file is non-empty. Failed concatenation removes the temp
file and never modifies a pre-existing destination.

## Suggested checks

```bash
go build ./...
go vet ./...
go test -race ./...
```

Scenario coverage worth exercising with fake tools: download failure,
over-duration clip, pre-existing truncated cache file, concat failure with a
pre-placed output, fewer clips than `--limit`, `taskkill -F /T` / `kill -9`
mid-download, and two concurrent generations sharing `--cache-dir`
(including `-K 0`).
