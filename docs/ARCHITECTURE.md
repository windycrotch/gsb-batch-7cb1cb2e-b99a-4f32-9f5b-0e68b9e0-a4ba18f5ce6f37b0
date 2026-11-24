# Architecture

ambivox is two processes.

- **`ambivox`** (Go) faces the network: it serves the web client, runs the
  signalling WebSocket and terminates WebRTC.
- **`ambivox-media`** (Rust, with assembly kernels and a C++ inference engine
  linked in) does everything that touches audio samples.

They talk over a Unix socket using the framing described in
[PROTOCOL.md](PROTOCOL.md). The split keeps the garbage-collected process away
from the real-time path, lets either side restart without the other, and makes
the media engine testable without a browser.

## Life of a sentence

Alice speaks French, Bob listens in English.

1. **Capture.** Alice's browser sends Opus over SRTP. Pion decrypts it and the
   server reads RTP packets from her track.
2. **Routing.** The room looks up the other participant. Same language: the
   payload is written straight to Bob's outgoing track. Different language: the
   payload and its RTP sequence number go to the media engine on the stream that
   was opened for the pair (`fr → en`).
3. **Decode.** The engine decodes Opus to 48 kHz mono. Sequence gaps are filled
   with Opus packet-loss concealment (at most ten frames); packets that arrive
   late are dropped. A delivery stall shorter than 400 ms is treated as jitter:
   the audio resumes where it left off instead of having silence spliced in.
4. **Resample.** A polyphase windowed-sinc filter brings 48 kHz down to the
   16 kHz the recogniser expects. Its inner product is one of the assembly
   kernels.
5. **Segment.** An energy-based voice activity detector tracks the noise floor
   and cuts the stream into utterances: 40 ms of speech opens one, 600 ms of
   silence closes it, 300 ms of pre-roll keeps the first consonant, and anything
   longer than 15 s is cut so latency stays bounded. A loud frame only counts
   as a voice when it is also periodic, with a pitch between 70 and 400 Hz; an
   utterance with less than 60 ms of it is dropped, so knocks and doors never
   reach whisper, which would hear "thank you" in them.
6. **Preview.** While the utterance is still open, the engine transcribes what
   has been said so far, every 800 ms once it is 600 ms long, and translates
   that draft. The drafts are sent as partial captions so both people watch the
   sentence being written. They never produce audio.
7. **Interpret.** A worker thread runs the C++ engine on the utterance:
   transcribe, translate, synthesise. Each stream's utterances are processed
   strictly in order even though workers are shared between streams. The
   transcript and the translation are sent back as captions as soon as each is
   known.
8. **Pace.** The synthesised audio is resampled to 48 kHz, queued, encoded to
   20 ms Opus frames and released on a 20 ms clock, so the server can forward
   frames as they arrive without buffering.
9. **Deliver.** The server writes each frame to Bob's track and both browsers
   render the captions.

## The media engine

```
socket reader ──► per-stream "listen" thread ──► job queue ──► inference workers
                   decode · resample · VAD                       (shared pool)
                                                                      │
socket writer ◄── per-stream "speak" thread ◄── playout buffer ◄──────┘
                   encode · pace
```

- **Bounded everywhere.** The input queue holds 256 packets per stream, the
  output queue 4096 frames per connection, the playout buffer 20 s of audio.
  When a bound is hit the oldest or newest item is dropped and a counter is
  incremented; nothing blocks the audio path and nothing grows without limit.
- **Ordering.** A stream is placed on the run queue at most once. A worker takes
  one utterance from it and puts the stream back if more are waiting, which
  gives per-stream ordering with a shared pool and no per-stream thread for
  inference.
- **Drafts never delay the sentence.** A stream has at most one draft queued or
  running. When the utterance ends, any draft still waiting in the queue is
  discarded and the final transcription takes its place, so a slow machine
  shows fewer drafts rather than later audio.
- **Shutdown.** Closing a stream flushes the open utterance, waits for pending
  jobs and for the playout buffer to drain, then confirms. Dropping the
  connection instead stops every stream immediately.

### Assembly kernels

`crates/dsp/src/kernels/` holds three routines, each in SSE2, AVX2+FMA and NEON:

| Kernel | Used by |
|---|---|
| `dot_f32` | every output sample of the resampler, and frame energy for voice detection |
| `pcm16_to_f32` | WAV input |
| `f32_to_pcm16` | WAV output, with saturation, round-half-to-even and NaN → 0 |

The files contain only instructions. Rust wraps each one in a naked function,
which supplies the symbol and the calling convention for the target platform,
and picks AVX2 or SSE2 at run time. Every kernel is tested against a scalar
implementation over awkward lengths, unaligned slices and edge values; the
integer conversions must match bit for bit.

### Inference engine

`engine/` exposes a small C ABI (`engine/include/ambivox/engine.h`): open and
close an engine, transcribe, translate, synthesize. `ambivox_translate_stream`
also reports the translation as it grows, so the voice can start before it is
finished. Two back ends implement them:

- **`sim`** turns text into a sequence of tones and back. Each byte becomes two
  differentially encoded tones out of seventeen, framed with a length and a
  CRC-8. The signal survives Opus, resampling and gain changes, which is what
  makes the end-to-end tests exact: the test asserts on the text decoded from
  the audio that came out of the far end.
- **`sussurro`** uses whisper.cpp for recognition, sussurro.cpp for translation
  and sherpa-onnx with Piper voices for synthesis. With `--translator`,
  translation runs instead on a language model through llama.cpp, prompted the
  way TranslateGemma expects. llama.cpp is pinned to the revision whose ggml is
  the one sussurro.cpp and whisper.cpp build on, so the three share a single
  copy. The language model keeps what it has already read, so the instructions
  of its prompt are not read again for every sentence. Models load on first use
  and stay resident; each model is guarded by its own mutex.

The engine is built by CMake, driven from the Rust build script. The simulated
back end links statically; the real one is a shared library so that its own
dependencies stay out of the Rust link line.

## The server

- **Rooms** hold at most two peers. Whenever membership, a participant's
  language, a mode, or the engine link changes, the room recomputes which
  translation streams should exist and opens or closes them to match. A
  replaced stream is retired as a unit: the room drops its handler first, so
  speech and captions the engine still returns from it are neither delivered
  nor counted, and only then asks the engine to finish it.
- **Signalling** is one WebSocket per participant: `join`, one `offer`/`answer`
  exchange with complete ICE candidates, then events. See
  [PROTOCOL.md](PROTOCOL.md).
- **Engine link.** The server keeps one multiplexed connection to the engine and
  reconnects with backoff. If the engine restarts mid-call the participants are
  told, their streams are reopened when it returns, and the WebRTC sessions stay
  up throughout.
- **Same-language calls** never touch the engine.
- **Invitations.** A room only lets in people who present a token signed by
  the server for that room, and the token expires. The server keeps no list of
  rooms or links: the signature is the whole record.
- **Echo guard.** While the listener hears a translation, the speaker's
  microphone may pick it up from the listener's loudspeaker, and the engine
  would translate it back. Until that speech has played, plus 600 ms for
  buffering, the speaker's packets are replaced by Opus silence before they
  reach the engine. Browsers cancel the echo of their own loudspeaker; this
  covers the one next to them.
- **Same room mode.** With both devices side by side, each microphone hears
  both people, and the engine would translate the wrong one. Browsers report
  the loudness of every packet in an RTP header extension; the louder
  microphone takes the floor, and the other one is replaced by silence until
  600 ms after the speaker stops. A clearly louder voice, by 6 dB or more,
  takes the floor at once, so the right person wins even when the wrong
  microphone was heard first. The page also turns automatic gain control off,
  which would otherwise even out the difference.
- **Headphones mode.** With nothing coming out of a loudspeaker the echo guard
  is not needed, so the engine may speak before the sentence ends. It compares
  consecutive partial transcripts and treats the words they agree on as
  settled, minus the last one, which may still change. Once a clause ends with
  punctuation after at least three settled words, or eight settled words go by
  without one, that part is translated and spoken while the speaker carries
  on. At the end of the utterance only the rest is spoken; the final captions
  keep the whole sentence, translated in one piece.
- **One UDP port.** With `-udp-port`, every call shares a single socket and
  ICE tells the connections apart, so a firewall or a container needs one port
  opened instead of a range.

## What is deliberately simple

- Voice detection is energy-based. It is predictable and cheap, and wrong in
  loud rooms; a model-based detector would slot in behind the same interface.
- Drafts are recomputed from the start of the utterance each time, which is
  simple and keeps whisper's context, at the cost of repeated work on long
  sentences. Outside headphones mode, audio still waits for the end of the
  utterance.
- One engine process serves one server process. Scaling out means running more
  pairs behind a load balancer that keeps a room on one pair.
