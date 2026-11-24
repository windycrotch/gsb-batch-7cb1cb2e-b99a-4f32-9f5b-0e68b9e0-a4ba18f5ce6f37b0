# Changelog

## 0.8.0

- Model profiles pick every model in one setting: `light` (whisper base,
  opus), `balanced` (whisper small, opus; the default) and `quality` (whisper
  large-v3-turbo, TranslateGemma 12B). `PROFILE` for `make`, `AMBIVOX_PROFILE`
  for Docker and the setup script, `--profile` for `ambivox-media`; `--whisper`
  and `--translator` still override either. The setup script downloads
  TranslateGemma for the quality profile and checks it against a pinned
  checksum. New `make models`.
- `--threads` defaults to the cores available divided by the workers, at most
  8, instead of 4. On a CPU, whisper small went from 1.57 s to 0.97 s per
  sentence.
- The engine log keeps to what matters: whisper, ggml and the opus decoder no
  longer print their loading details and per-sentence statistics, only their
  errors. sussurro.cpp moves to a revision that also drops words opus does not
  know instead of writing them as `⁇`. The pinned revisions of sussurro.cpp and
  llama.cpp now take effect in an existing build directory too.
- `scripts/record-bench.sh LANG` records your voice for the bench, one sentence
  at a time, into `recordings/`.

## 0.7.0

- `ambivox-media bench` and `make bench` measure a model setup on a set of
  sentences: word error rate of recognition, chrF of the translation against a
  reference (the same formula as sacreBLEU), and the time each step takes.
  `bench/sentences.txt` holds 56 everyday sentences across every supported
  language pair. Recordings of your own voice replace the synthetic one.
  Numbers spelled out or in digits and English contractions count the same,
  and a pair whose models are missing is skipped. Translations far from the
  reference are listed with what came out.
- Translation can run on a language model: `--translator NAME` loads
  `NAME.gguf` from the model directory with llama.cpp and prompts it as
  TranslateGemma expects. One model covers every language pair. `opus`, the
  per-pair models, stays the default. `TRANSLATOR` for `make`,
  `AMBIVOX_TRANSLATOR` for Docker. The engine's C ABI gains `translator` at the
  end of `ambivox_config`, and the Rust `Engine::open` takes an `Options`.
- With a language model, the voice no longer waits for the whole translation.
  The model hands over its text as it writes, and each clause of three words or
  more that ends in a punctuation mark is spoken while the rest is translated.
  The model also keeps the instructions of its prompt from one sentence to the
  next instead of reading them again. New C function `ambivox_translate_stream`,
  new engine counter `streamed_clauses`, new `voice p50` column in the bench:
  the time until the first words of the translated voice are ready.

## 0.6.0

- Same room mode. With both devices side by side, each microphone also hears
  the other person, and ambivox translated whatever reached it. **Same room**
  in the call keeps only the microphone closer to whoever speaks, using the
  loudness browsers report in every packet. New message `same-room`, new field
  `sameRoom` in `joined`, new metric
  `ambivox_rtp_packets_same_room_muted_total`.
- Headphones mode. When both people wear headphones, **Headphones** in the
  call makes the translated voice start before the sentence ends: as soon as
  a clause is settled across live transcripts, or after eight settled words,
  it is translated and spoken while the speaker goes on. The echo guard is off
  for that call. New message `headphones`, new field `headphones` in `joined`,
  new `ambivox-media serve --early-words`, new engine counter `early_chunks`.
  The engine protocol moves to version 3: OPEN gains a flags byte, so the
  server and the engine must be upgraded together.
- Noises no longer turn into sentences such as "merci" or "thank you". The
  voice detector only passes sounds with a pitch, so knocks and doors never
  reach whisper, and recognition drops what whisper takes for silence, as
  OpenAI's reference decoder does, and suppresses non-speech tokens.
  `SegmenterConfig` gains `min_pitched_ms`.
- The whisper model can be chosen: `--whisper` for `ambivox-media`,
  `AMBIVOX_WHISPER` for Docker and the setup script, `WHISPER` for `make run`.
  `small` stays the default; `large-v3-turbo` is more accurate on a fast
  machine. The engine's C ABI gains `asr_model` at the end of
  `ambivox_config`.
- Building needs Rust 1.100 or later, and the engine is written in C++20.

## 0.5.0

- The server needs Go 1.26 or later; Go 1.24 is no longer supported upstream.
  Its dependencies move to their latest releases: Pion WebRTC 4.2.28, ICE
  4.4.9, DTLS 3.1.16 and the `golang.org/x` packages.
- Opus moves to libopus 1.6.1, through the `opus` 0.4 crate.
- sherpa-onnx moves to 1.13.8.
- The server code uses current Go idioms (`go fix`).
- CI tests with the latest stable Go, and the workflows use the current major
  versions of their actions.
- The model setup image runs Python 3.13.
- Fixed: the published 0.4 images stopped at start-up with an illegal
  instruction on most machines, because whisper.cpp was tuned for the CPU of
  the machine that built them. `AMBIVOX_PORTABLE=1` now builds the engine for
  the whole architecture, and the images use it: any x86-64 CPU with AVX2
  (2013 onwards) and any arm64 CPU with ARMv8.2 dot product and half-precision
  instructions, such as Apple silicon, Raspberry Pi 5 or AWS Graviton.

## 0.4.0

- Ready-made images. Every release publishes `ghcr.io/whispem/ambivox` for
  x86-64 and arm64, so `docker compose up` downloads ambivox instead of
  compiling it. The image runs the real speech backend by default and also
  carries the simulated one (`AMBIVOX_BACKEND=sim`).
- `docker compose run --rm models` downloads and prepares the speech models in
  a volume, with nothing to install on the machine.
- The images carry the licences of everything compiled into them.
- `compose.build.yaml` builds the images from the sources;
  `compose.sussurro.yaml` is gone.
- `scripts/setup-sussurro.sh` gains a `converter` step and can use an existing
  Python (`AMBIVOX_CONVERT_PYTHON`).

## 0.3.0

- Runs on a local network with no Internet. `docker compose up` starts the
  media engine and the server; `make lan` does the same without Docker.
- `-tls-self-signed` serves HTTPS with a certificate generated for the
  machine's addresses, kept in `-data-dir` and renewed when they change. The
  server prints the addresses to open and the certificate fingerprint.
- Rooms by invitation. Starting a call creates a random room and a signed link
  that expires after 24 hours (`-invite-ttl`); joining without it is refused.
  `-open-rooms` restores rooms joinable by name.
- `AMBIVOX_ACCESS_KEY` restricts who can start a call.
- `-udp-port` carries every call over a single UDP port.
- Echo guard: while a speaker's translation plays to the listener, the
  speaker's microphone is silenced, which stops two devices in the same room
  from translating each other's loudspeaker in a loop (`-echo-guard`, on by
  default). New metric `ambivox_rtp_packets_muted_total`.
- New endpoint `POST /rooms`; `join` carries a `token`; `/config` reports
  `inviteOnly` and `accessKey`.
- `make offline` runs complete calls in a network namespace with only
  loopback, in CI on Linux.
- The web client starts calls and shares invitation links instead of asking for
  a room name.
- `scripts/setup-sussurro.sh` supports Linux on arm64.
- Documentation: local network setup, privacy, running behind your own reverse
  proxy.

## 0.2.0

- Live captions. While someone speaks, both participants see the sentence being
  transcribed and translated, then the final version replaces the draft. Drafts
  are refreshed every 800 ms (`ambivox-media serve --partials-ms`, `0` turns
  them off) and never delay the translated voice.
- The engine protocol moves to version 2: the CAPTION kind gains a partial bit.
  Upgrade `ambivox` and `ambivox-media` together.
- Captions sent to the browser carry a `final` field.
- New metrics: `ambivox_partial_captions_total`, and the engine counters
  `partials`, `partials_skipped` and `partial_ms`.
- The simulated backend speaks word by word, so live captions can be tested
  without models.
- Fixed: the join form stayed visible during a call.
- The Rust crates move to the 2024 edition.

## 0.1.0

- First release: translated two-person voice calls over WebRTC, with a Go
  server, a Rust media engine with x86-64 and AArch64 assembly kernels, and a
  C++ inference engine backed by whisper.cpp, sussurro.cpp and Piper.
