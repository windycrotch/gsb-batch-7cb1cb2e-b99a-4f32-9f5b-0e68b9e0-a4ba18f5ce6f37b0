# Protocols

## Server ↔ media engine

A stream of frames over a Unix socket. The server connects; the engine listens.

```
frame   := length:u32 type:u8 stream:u32 payload
length  := 5 + len(payload), at most 65536
```

All integers are big-endian. `stream` is chosen by the server and identifies one
direction of one call (one speaker, one target language). It is `0` for frames
that concern the whole connection.

### Server → engine

| Type | Name | Payload |
|---|---|---|
| `0x01` | OPEN | `source_len:u8 source target_len:u8 target flags:u8` — language codes, then flags |
| `0x02` | AUDIO | `sequence:u16 opus` — one Opus packet and its RTP sequence number |
| `0x03` | CLOSE | empty — finish the stream gracefully |

### Engine → server

| Type | Name | Payload |
|---|---|---|
| `0x00` | HELLO | `version:u16 backend` — sent once, first; current version is `3` |
| `0x81` | SPEECH | `opus` — one 20 ms Opus packet (48 kHz mono) of translated speech |
| `0x82` | CAPTION | `kind:u8 utterance:u32 text` — see below |
| `0x83` | ERROR | `text` — a problem with this stream; the stream stays open unless CLOSED follows |
| `0x84` | CLOSED | empty — the stream is gone and its id may be forgotten |
| `0x85` | STATS | `json` — engine-wide counters, every two seconds |

Text is UTF-8. A frame with an unknown type, a bad length or invalid UTF-8 is a
protocol error and the receiver drops the connection.

### Rules

- The engine answers a refused OPEN (unsupported language, duplicate id) with
  ERROR followed by CLOSED.
- AUDIO for an unknown stream is ignored.
- After CLOSE the engine finishes the utterance in progress, plays out what is
  queued, then sends CLOSED. SPEECH and CAPTION frames may still arrive in
  between.
- SPEECH frames are released in real time, one every 20 ms while there is
  something to say; the server forwards them without buffering.
- Captions for an utterance share its `utterance` number, which counts from zero
  per stream. The transcript always precedes the translation.
- In the CAPTION `kind`, bit 0 says translation (1) or transcript (0) and bit 1
  marks a partial caption: `0` transcript, `1` translation, `2` partial
  transcript, `3` partial translation. Partial captions describe the utterance
  still being spoken; each replaces the previous one with the same number, and
  the final pair (kinds `0` and `1`) replaces them all. Partials may be skipped
  under load, never the final pair.
- OPEN flags: bit 0 asks the engine to speak early, before the utterance ends,
  as soon as the start of the sentence has settled across partial
  transcripts. The other bits must be zero. The final captions still carry the
  whole sentence; only the speech is split.
- Version 2 added partial captions, version 3 the OPEN flags. Both processes
  refuse a peer that speaks another version.
- Dropping the connection closes every stream immediately.

`testdata/protocol_vectors.txt` holds one encoded example of each frame. The
Rust and Go test suites both check their codec against it.

## Browser ↔ server

JSON text messages over a WebSocket at `/ws`. The WebSocket must come from the
same origin as the page unless the server was started with `-allow-origin`.

### Client → server

```json
{"type": "join", "room": "3f9c…", "lang": "fr", "name": "Em", "token": "tqk2a8.Jdy…"}
{"type": "offer", "sdp": "…"}
{"type": "lang", "lang": "es"}
{"type": "same-room", "sameRoom": true}
{"type": "headphones", "headphones": true}
{"type": "leave"}
```

`join` must be the first message and arrive within ten seconds. Room names are
1–64 characters from `A-Z a-z 0-9 _ -`. `lang` is one of `en`, `fr`, `es`, `it`.
`token` is the invitation for that room; it is ignored when the server runs
with `-open-rooms`.
The offer carries one `sendrecv` audio section with all ICE candidates already
gathered; there is exactly one offer per session. The server accepts the
`urn:ietf:params:rtp-hdrext:ssrc-audio-level` header extension, which browsers
offer by default; same room mode needs it. `same-room` turns that mode on or
off for both participants. `headphones` does the same for headphones mode: the
server reopens the translation streams with the early flag and stops the echo
guard, which would otherwise silence the speaker while the start of their
sentence is already playing. `lang` changes the language this participant
speaks, mid-call; the server replaces the translation streams for both
directions and tells everyone with a `lang` event. An unsupported language is
answered with an `error` and changes nothing.

Whatever causes it — a join, a leave, a language change, a headphones toggle, a
media engine reconnect — the room keeps exactly one translation stream in
effect per direction, matching the current pairing. The stream being replaced
is closed on the engine side, and anything it still returns (speech or
captions) is dropped and left out of the counters.

### Server → client

```json
{"type": "joined", "id": "9f2c…", "room": "3f9c…", "lang": "fr", "name": "Em",
 "connected": true, "sameRoom": false, "headphones": false, "backend": "sussurro", "languages": ["en", "fr", "es", "it"],
 "peer": {"id": "41aa…", "name": "Bob", "lang": "en"},
 "iceServers": [{"urls": ["stun:stun.example.org:3478"]}]}
{"type": "answer", "sdp": "…"}
{"type": "peer-joined", "peer": {"id": "41aa…", "name": "Bob", "lang": "en"}}
{"type": "peer-left", "id": "41aa…"}
{"type": "lang", "id": "9f2c…", "lang": "es"}
{"type": "caption", "from": "9f2c…", "kind": "source", "utterance": 3, "final": false, "text": "bonjour à"}
{"type": "caption", "from": "9f2c…", "kind": "translation", "utterance": 3, "final": false, "text": "hello to"}
{"type": "caption", "from": "9f2c…", "kind": "source", "utterance": 3, "final": true, "text": "bonjour à tous"}
{"type": "caption", "from": "9f2c…", "kind": "translation", "utterance": 3, "final": true, "text": "hello everyone"}
{"type": "same-room", "sameRoom": true}
{"type": "headphones", "headphones": true}
{"type": "media", "connected": false, "backend": ""}
{"type": "error", "message": "…"}
```

`peer` and `iceServers` are omitted when there is nothing to report. Captions go
to both participants; `from` is the speaker. A caption with `"final": false` is
a live draft that the next caption for the same `from`, `utterance` and `kind`
replaces. `same-room` and `headphones` tell both participants that one of them changed
that mode. `lang` says that `id` now speaks `lang`, so both sides can relabel
the call. `media` announces that the translation engine went away or came back.

A join that cannot be honoured (missing, forged or expired invitation, full
room, room limit, invalid name or language) gets an `error` and the socket is closed with status 1008. Errors after joining
leave the session open. On shutdown the server closes with status 1001.

### Creating a room

~~~http
POST /rooms
Authorization: Bearer <access key>
~~~

The header is only needed when the server has an access key; a wrong or
missing one gets `401`. The answer is `201` with:

~~~json
{"room": "3f9c…", "token": "tqk2a8.Jdy…", "expires": "2026-11-21T09:17:44Z"}
~~~

The room is a random 32-character hexadecimal name and exists once someone
joins it. The token is the expiry time in base 36, a dot, and the first 18
bytes of an HMAC-SHA256 over the room and that expiry, in unpadded base64url.
With `-open-rooms`, `token` and `expires` are absent.

The web client shares the invitation as `https://host/#room=…&t=…`.
