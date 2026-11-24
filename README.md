# ambivox

Voice calls where each person speaks their own language and hears the other in
theirs — self-hosted, with no cloud service in the loop.

One person starts a call and sends a link; the other opens it, picks a
language and talks. ambivox listens to each speaker, transcribes, translates,
re-synthesises the sentence in the listener's language and plays it over
WebRTC, with live captions on both sides. Speech recognition, translation and
synthesis all run on your own machine, and a call needs no Internet connection
at all: a laptop and a Wi-Fi network are enough.

Supported languages: English, French, Spanish and Italian, in every direction.

```
 browser A ──WebRTC──┐                              ┌──WebRTC── browser B
   (fr)              │                              │             (en)
              ┌──────▼──────────────────────────────▼──────┐
              │  ambivox            Go                     │
              │  signalling · rooms · ICE/DTLS/SRTP        │
              └──────────────────┬─────────────────────────┘
                                 │ Unix socket, Opus frames
              ┌──────────────────▼─────────────────────────┐
              │  ambivox-media      Rust                   │
              │  jitter/loss handling · resampling · VAD   │
              │  scheduling · pacing        ┌────────────┐ │
              │                             │ x86-64 and │ │
              │                             │ AArch64    │ │
              │                             │ assembly   │ │
              │                             └────────────┘ │
              │  ┌──────────────────────────────────────┐  │
              │  │ engine              C++              │  │
              │  │ whisper.cpp → sussurro.cpp → Piper   │  │
              │  └──────────────────────────────────────┘  │
              └────────────────────────────────────────────┘
```

## Why four languages

Each one sits on a real boundary:

| Language | Where | Why |
|---|---|---|
| Go | `server/` | WebRTC transport and signalling, built on [Pion](https://github.com/pion/webrtc). |
| Rust | `crates/` | The real-time audio path: no garbage collector, explicit threads, bounded queues. |
| Assembly | `crates/dsp/src/kernels/` | The inner loops of resampling, level metering and sample conversion (SSE2, AVX2+FMA, NEON), each checked against a scalar reference. |
| C++ | `engine/` | Inference. It wraps whisper.cpp, [sussurro.cpp](https://github.com/whispem/sussurro.cpp) and sherpa-onnx behind a small C ABI. |

## On your local network

All it takes is Docker and the `compose.yaml` file from this repository:

~~~bash
curl -fsSLO https://raw.githubusercontent.com/whispem/ambivox/main/compose.yaml
echo AMBIVOX_HOST=192.168.1.20 > .env    # this machine's address on your network
docker compose run --rm models           # once: downloads and prepares the models
docker compose up
~~~

Nothing is compiled: ready-made images for x86-64 and arm64 are published with
every release. Preparing the models downloads a few gigabytes and takes a
while, but only the first time; after that ambivox needs no Internet at all.

On macOS, `ipconfig getifaddr en0` prints that address; on Linux,
`hostname -I`. Then open `https://192.168.1.20:8443` on any phone or computer
connected to the same network. The certificate is generated on the spot, so
each browser asks once whether to trust it. Choose **Start a call**, and send
the invitation link to the other person.

Invitation links expire after 24 hours. To decide who may start calls at all,
add `AMBIVOX_ACCESS_KEY=…` to `.env`. Everything about this setup, without
Docker too, is in [docs/OPERATIONS.md](docs/OPERATIONS.md#on-a-local-network).

## Quick start from source

Requirements: Rust 1.100+, Go 1.26+, CMake 3.16+ (3.28+ for the real backend) and a
C++20 compiler.

```bash
make check    # format, lint, unit tests, and a full call end to end
make run      # start the media engine and the server on http://127.0.0.1:8080
make lan      # the same over HTTPS, reachable from other devices on the network
```

`make run` uses the **simulated backend**, which needs no model: it "speaks"
text as a sequence of tones and "recognises" those tones again, with a small
built-in dictionary standing in for translation. It exists so that the whole
pipeline — WebRTC, codec, resampling, voice detection, scheduling — can be
tested deterministically. It does not understand human speech.

## Real speech

The real backend uses whisper.cpp for recognition, sussurro.cpp (OPUS-MT) for
translation and Piper voices through sherpa-onnx for synthesis. Translation can
instead run on TranslateGemma through llama.cpp; see
[docs/OPERATIONS.md](docs/OPERATIONS.md#model-directory).

```bash
make models
make run BACKEND=sussurro
```

`make models` downloads the runtime and several GB of models. `PROFILE=light`,
`balanced` (the default) or `quality` picks them for both commands; see
[docs/OPERATIONS.md](docs/OPERATIONS.md#model-directory).

Open `http://127.0.0.1:8080`, start a call, open the invitation link in a
second tab (or on another device, with `make lan`), choose two different
languages, and talk.

Docker uses this backend by default, with the models prepared by
`docker compose run --rm models`. `AMBIVOX_BACKEND=sim docker compose up`
starts the simulated one instead.

If you already have a sussurro.cpp checkout with its models, point ambivox at
it instead of downloading again:

```bash
ln -s /path/to/sussurro.cpp/third_party/whisper.cpp/models/ggml-small.bin \
      /path/to/sussurro.cpp/models/
export AMBIVOX_SHERPA_DIR=/path/to/sussurro.cpp/third_party/sherpa-onnx
make run BACKEND=sussurro MODELS=/path/to/sussurro.cpp/models
```

## Testing

| Command | What it covers |
|---|---|
| `make test-engine` | C++ engine: tone codec, simulated backend, C ABI error handling. |
| `make test-rust` | Assembly kernels against the scalar reference (with and without the `asm` feature), resampler, voice detection, Opus wrapper, wire protocol, pipeline ordering, loss handling, pacing. |
| `make test-go` | Wire protocol against the same vectors as Rust, engine link and reconnection, rooms, signalling, HTTP endpoints, with real WebRTC connections and the race detector on. |
| `make e2e` | Two WebRTC participants, the Go server and the real `ambivox-media` binary: a spoken phrase goes in, live captions grow while it is spoken, and its translation comes out as audio and final captions. |
| `make offline` | The end-to-end calls again, inside a network namespace with nothing but loopback (Linux). It fails if any other interface is up, so a pass shows that a call needs no network beyond the machine. |
| `make browser-check` | A call started from the actual web client in headless Chrome over self-signed HTTPS: access key, invitation link, a forged link refused, translated captions on both sides (needs Chrome and Node.js; not part of `make check`). |

Continuous integration runs `make check` on Linux x86-64 and macOS arm64, so
both assembly back ends are exercised, and `make offline` on Linux. The model-backed path is compiled but
not run in CI because of the size of the models. Pushing a tag builds and
publishes the Docker images; the same workflow can be started by hand to
publish an `edge` image from any commit.

## Layout

```
engine/            C++ inference engine and its C ABI
crates/dsp/        Rust: SIMD kernels, polyphase resampler, voice activity segmenter
crates/engine/     Rust: safe bindings to the C++ engine
crates/media/      Rust: ambivox-media, the real-time pipeline and its socket server
server/            Go: ambivox, the WebRTC server, plus the embedded web client
testdata/          Protocol vectors shared by the Rust and Go test suites
docs/              Architecture, wire protocol, operations
scripts/           Model setup and the headless browser check
compose.yaml       The media engine and the server for Docker, plus the model setup
```

## Limits worth knowing

- A room holds two participants, and only people holding its invitation link
  can join.
- Nothing is kept: no accounts, no recordings, no transcripts. See
  [docs/OPERATIONS.md](docs/OPERATIONS.md#privacy).
- Captions are written live while someone speaks; the translated voice follows
  once the speaker pauses, typically one to three seconds later depending on
  hardware. If you both wear headphones, press **Headphones**: the voice then
  starts at the first comma or after a few words, while the sentence goes on.
  Translating a sentence in pieces can read less naturally than in one go.
- When both participants pick the same language the audio is relayed untouched.
- You can change the language you speak during a call with the **I speak**
  selector: the translation streams for both directions are replaced at once,
  and whatever the old ones were still saying is dropped.
- While your translation is playing to the other person, your microphone is
  silenced, so two devices in the same room cannot feed each other's voice
  back in a loop. Wait for it to finish before going on. **Headphones** turns
  this off for the call, `-echo-guard=false` for the whole server.
- With both devices in one room, press **Same room**: each microphone also
  hears the other person, and only the one closer to whoever speaks is kept.
- The assembly kernels cover x86-64 and AArch64; other targets use the scalar
  Rust implementation (`--no-default-features` forces it anywhere).

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — how audio flows and why it is built this way
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the server ↔ media engine wire format and the signalling messages
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — local network setup, privacy, configuration, reverse proxies, metrics, troubleshooting

## License

MIT, see [LICENSE](LICENSE). Models and third-party runtimes keep their own
licences: whisper.cpp, llama.cpp and ggml (MIT), OPUS-MT models (CC-BY 4.0),
TranslateGemma if you choose it (Gemma terms), SentencePiece and sherpa-onnx
(Apache-2.0), Piper voices (see each model card), Opus (BSD), Pion (MIT).
