# Media Processing HTTP API

An HTTP API for processing video, audio, and images with FFmpeg. A request supplies named inputs and one or more FFmpeg command argument lists. Results can be uploaded to S3, returned as Base64 JSON, served as a completed file, or streamed from FFmpeg while it is running.

## Endpoint

`POST /v1/process`

```bash
curl http://localhost:8080/v1/process \
  --request POST \
  --header "Content-Type: application/json" \
  --data '{
    "input": {
      "input.mp4": { "http": "https://example.com/video.mp4" }
    },
    "commands": [
      ["-i", "input.mp4", "-vn", "output.mp3"]
    ],
    "output": { "base64": true }
  }'
```

The request body has this shape:

```json
{
  "s3Config": {},
  "input": {},
  "commands": [],
  "output": {}
}
```

Unknown fields and multiple JSON values in one request body are rejected.

## Inputs

`input` maps a filename to exactly one source. Filenames cannot contain a directory path.

| Field       | Purpose                                                                                                                    |
|-------------|----------------------------------------------------------------------------------------------------------------------------|
| `http`      | Download an accessible HTTP or HTTPS URL.                                                                                  |
| `s3`        | Download an object such as `s3://bucket/path/input.mp4`.                                                                   |
| `base64`    | Decode Base64 content into the named input file.                                                                           |
| `temporary` | Reserve a filename for an intermediate file created by a command. It is not fetched and is excluded from returned outputs. |

```json
{
  "input.mp4": {
    "http": "https://example.com/video.mp4"
  },
  "intermediate.wav": {
    "temporary": true
  }
}
```

Independent inputs are fetched concurrently. If the client disconnects, active HTTP downloads, S3 transfers, and FFmpeg processes are canceled.

## Commands

`commands` is a non-empty array of FFmpeg argument lists. Commands run sequentially in an isolated job directory, so a later command can use a file produced by an earlier command. The service adds `-hide_banner`; do not include the `ffmpeg` executable itself.

```json
[
  ["-i", "input.mp4", "-vn", "intermediate.wav"],
  ["-i", "intermediate.wav", "-codec:a", "libmp3lame", "output.mp3"]
]
```

## Output modes

At least one output mode is required. S3 and Base64 can be combined. Inline output is exclusive because its response body is media rather than JSON.

### S3

```json
{
  "s3": "s3://my-bucket/outputs/"
}
```

Each output file is uploaded beneath the supplied prefix. The JSON response contains its resulting S3 URL.

### Base64 JSON

```json
{
  "base64": true
}
```

```json
{
  "results": {
    "output.mp3": {
      "base64": "..."
    }
  }
}
```

Base64 responses hold encoded output in memory and are best suited to small files. Prefer S3 or HTTP output for larger media.

### Completed file response

```json
{
  "inlineContentType": "audio/mpeg"
}
```

FFmpeg finishes first, then the first output file is served with the supplied content type. No JSON body is returned.

### Live FFmpeg stream

Set `stream` to `true` and make the final command write to standard output with `pipe:1`. An explicit `-f` is required because a pipe has no filename extension from which FFmpeg can infer a format.

```bash
curl http://localhost:8080/v1/process \
  --request POST \
  --header "Content-Type: application/json" \
  --dump-header stream.headers \
  --output output.mp3 \
  --data '{
    "input": {
      "input.wav": { "http": "https://example.com/audio.wav" }
    },
    "commands": [
      ["-i", "input.wav", "-f", "mp3", "pipe:1"]
    ],
    "output": {
      "stream": true,
      "inlineContentType": "audio/mpeg"
    }
  }'
```

The body begins as FFmpeg produces bytes and is not written to a temporary output file. The response ends with these HTTP trailers:

| Trailer           | Value                                                   |
|-------------------|---------------------------------------------------------|
| `X-Ffmpeg-Status` | `ok` when FFmpeg exits successfully, otherwise `error`. |
| `X-Ffmpeg-Bytes`  | Number of media bytes written to the response.          |

The initial status is `200 OK` before FFmpeg completes. A later FFmpeg failure therefore produces a truncated body with an `error` trailer rather than a different HTTP status. Clients and reverse proxies used for this mode must preserve HTTP trailers.

## S3 configuration

`s3Config` is required when any input or output uses S3.

```json
{
  "s3Config": {
    "endpoint": "s3.amazonaws.com",
    "region": "us-east-1",
    "accessKey": "AKIA...",
    "secretKey": "SECRET...",
    "useSSL": true
  }
}
```

The endpoint must not include a URL scheme or bucket name. `useSSL` defaults to `true`. AWS S3 and path-style S3-compatible services such as MinIO, DigitalOcean Spaces, and Cloudflare R2 are supported.

## Request lifecycle

Every request owns one temporary directory and one request-scoped context.

- Inputs download concurrently. If any download fails, its sibling downloads are canceled and FFmpeg never starts.
- A client disconnect cancels all in-flight downloads, S3 transfers, and the running FFmpeg process. Once canceled, the request performs no new S3 upload or Base64 encoding, so partially written files are never returned as results.
- Each request is isolated: canceling one request never cancels another request's downloads or FFmpeg processes and never removes another request's directory.
- The job directory is removed on every outcome — success, validation/processing failure, and cancellation — before the handler returns (and therefore before the HTTP response finishes). JSON responses are written only after removal; inline and streamed responses finish their file/pipe work first and remove the directory immediately afterwards.

## Tests

```bash
go test ./...
```

The lifecycle tests are hermetic and need no FFmpeg install: they re-execute the test binary as a controllable fake FFmpeg process, so cancellation still goes through the real `os/exec` process path, and use local `httptest` servers for downloads, S3 endpoints, and real TCP client disconnects. They cover successful collection (temporary inputs excluded), download failure with sibling cancellation, client disconnect during FFmpeg, the partial-output collection race, directory cleanup, and two concurrent requests with one canceled.

To additionally run the real-FFmpeg smoke test, put `ffmpeg` on `PATH` or set `LIFECYCLE_FFMPEG_BIN` to its executable path.

## Docker

The published image contains the API and FFmpeg:

```text
ghcr.io/aureum-cloud/ffmpeg-api:latest
```

```bash
docker run --detach --publish 8080:8080 ghcr.io/aureum-cloud/ffmpeg-api:latest
```

The API is then available at `http://localhost:8080`.

To build and test locally:

```bash
go test ./...
docker build --tag ffmpeg-api .
```

## Operations

Each request gets its own temporary directory and FFmpeg process. Job files are removed when the response finishes. The container runs as an unprivileged user. The service keeps no session state and can be scaled horizontally behind a load balancer.

FFmpeg controls its own worker threads. Supply `-threads 0` for automatic selection or a fixed value such as `-threads 4` in the relevant command.

The API does not provide authentication. Deploy it behind an authenticated gateway and apply network controls appropriate for an endpoint that fetches URLs and executes FFmpeg workloads.
