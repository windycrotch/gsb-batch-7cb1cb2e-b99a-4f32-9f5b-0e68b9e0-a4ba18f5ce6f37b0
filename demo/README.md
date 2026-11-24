# WebSocket Client Demo
To run this demo, build and start `ffmpegd` and load `http://localhost:8080/demo` in your browser.

```
go build -v && ./ffmpegd
```

http://localhost:8080/demo/

## Example
Use the JavaScript WebSocket API to connect and send an encode payload based on the [ffmpeg-commander](https://ffmpeg-commander.com) JSON format:

```javascript
var wsUri = "ws://localhost:8080/ws";
var payload = {
    "format": {
        "container": "mp4",
        "clip": false
    },
    "video": {
        "codec": "libx264",
        "preset": "veryslow",
        "pass": "1",
        "crf": 23,
        "pixel_format": "auto",
        "frame_rate": "auto",
        "speed": "auto",
        "tune": "none",
        "profile": "none",
        "level": "none",
        "faststart": false,
        "size": "source",
        "width": "1080",
        "height": "1920",
        "format": "widescreen",
        "aspect": "auto",
        "scaling": "auto",
        "codec_options": ""
    },
    "audio": {
        "codec": "copy",
        "channel": "source",
        "quality": "auto",
        "sampleRate": "auto",
        "volume": "100"
    },
    "filter": {
        "deband": false,
        "deshake": false,
        "deflicker": false,
        "dejudder": false,
        "denoise": "none",
        "deinterlace": "none",
        "brightness": "0",
        "contrast": "1",
        "saturation": "0",
        "gamma": "0",
        "acontrast": "33"
    }
};
websocket = new WebSocket(wsUri);
websocket.send(JSON.stringify({
    type: 'encode',
    input: 'input.mp4',
    output: 'output.mp4',
    payload: JSON.stringify(payload) // The payload is sent as a JSON string.
}));
```

Paths are relative to the directory `ffmpegd` was started in. Encodes run one at a time, in the order they are sent.

## Responses
The server sends progress about once a second while encoding:

```JSON
{"percent":34.68,"speed":"3.37x","fps":80.77}
```

Progress is measured against the length of the output, so it works for audio-only encodes and clips. For two-pass encodes, each pass is half of the total. `percent` stays below 100 until the encode is finished.

When the encode finishes:

```JSON
{"percent":100,"speed":"","fps":0}
```

If the encode fails, or a message can't be read, `err` holds the reason, usually ffmpeg's error output:

```JSON
{"percent":0,"speed":"","fps":0,"err":"Unknown encoder 'libaom-av1'"}
```

## Cancelling
Stop the running encode with:

```javascript
websocket.send(JSON.stringify({ type: 'cancel' }));
```

The server kills ffmpeg and replies:

```JSON
{"percent":0,"speed":"","fps":0,"cancelled":true}
```

## Task tracking (current protocol)
The messages above still work for clients that send no IDs. New clients can
give every encode a stable identity and track it through queued, running and
terminal states, even across multiple connected browsers and reconnects.

### Identifying the client
Pass a `client_id` on the connection (or send a `hello` message after
connecting). The same value identifies the browser when it reconnects.

```javascript
var clientId = localStorage.getItem("ffmpegd-client-id") || crypto.randomUUID();
localStorage.setItem("ffmpegd-client-id", clientId);
var ws = new WebSocket(wsUri + "?client_id=" + encodeURIComponent(clientId));
```

On connect the server sends a `welcome` and a `tasks` snapshot of everything
this client owns, including finished, failed and cancelled results:

```JSON
{"type":"welcome","client_id":"…"}
{"type":"tasks","tasks":[{"id":"task-1","state":"complete","percent":100}]}
```

### Submitting with an ID
Choose a unique `id` per encode. The server echoes that same `id` on every
event for the job, so progress, completion, failure and cancellation can
always be correlated.

```javascript
ws.send(JSON.stringify({
    type: 'encode',
    id: 'task-1',
    client_id: clientId,
    input: 'input.mp4',
    output: 'output.mp4',
    payload: JSON.stringify(payload)
}));
```

### Typed events
| Event       | Meaning                                              |
|-------------|------------------------------------------------------|
| `queued`    | Accepted and waiting for the worker.                 |
| `started`   | The worker picked it up; state is `running`.         |
| `progress`  | Periodic progress, about once a second.              |
| `complete`  | Finished successfully; `percent` is 100.             |
| `error`     | Failed; `err` holds the reason, state is `failed`.   |
| `cancelled` | Cancelled before or during encoding.                 |

```JSON
{"type":"progress","id":"task-1","state":"running","percent":34.68,"speed":"3.37x","fps":80.77}
{"type":"complete","id":"task-1","state":"complete","percent":100}
```

### Cancelling a specific task
Cancellation only affects a task owned by the calling client. A queued task is
removed and never runs (and produces no output); a running task is killed, its
partial output is deleted, and the worker continues with the next queued job.

```javascript
ws.send(JSON.stringify({ type: 'cancel', id: 'task-1', client_id: clientId }));
```

Cancelling another client's task, or an unknown ID, returns an `error` event.

### Disconnect and reconnect
When a client disconnects, its queued tasks are removed and its running task is
terminated with its temporary output cleaned up. On reconnect with the same
`client_id`, the `tasks` snapshot (or an explicit `status` query) returns the
latest state of the client's retained tasks:

```javascript
ws.send(JSON.stringify({ type: 'status', id: 'task-1', client_id: clientId }));
// {"type":"status","id":"task-1","state":"complete","percent":100}
```

Each task reaches exactly one terminal state (`complete`, `failed` or
`cancelled`); it can never report both completion and cancellation, and is
never executed twice. History is in-memory, so a server restart starts with no
retained tasks.

### Legacy compatibility
Clients that omit `id` and `client_id` keep the original ID-less behaviour
described above: progress/error/cancel arrive as bare `Status` objects without
a `type` field, and a `cancel` with no `id` stops the caller's own running
encode.