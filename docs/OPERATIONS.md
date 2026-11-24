# Operations

## Processes

Run the media engine first, then the server. Either can be restarted on its own:
the server reconnects to the engine with backoff, and calls in progress resume
translating when it returns.

```bash
ambivox-media serve --socket /run/ambivox/media.sock --backend sussurro --models /var/lib/ambivox/models
ambivox -listen :8080 -media-socket /run/ambivox/media.sock
```

`ambivox-media serve` runs a recognition self-test at start-up and exits with a
clear message if the model directory or the whisper model is missing.
Translation models and voices load the first time a language pair needs them.

## On a local network

ambivox is built to run on a network that has no Internet access: a home
Wi-Fi, a hotspot from a phone, a router with no uplink. Nothing in a call
leaves the machine that runs it, and the only thing a browser needs is to reach
that machine.

### With Docker

~~~bash
echo AMBIVOX_HOST=192.168.1.20 > .env
docker compose run --rm models
docker compose up
~~~

`AMBIVOX_HOST` must be the IP address the other devices use to reach this
machine. The server puts it in its certificate and advertises it as the media
address, since the address the container sees is not reachable from outside.
Keeping it in `.env` lets later commands (`docker compose down`, `logs`) find
it too. Port 8443 is published for both TCP (the page and signalling) and UDP
(all the audio of every call).

Three named volumes hold the state: `socket` for the Unix socket between the
two services, `data` for the certificate and the invitation key, so that
browsers keep trusting the server and links keep working across restarts, and
`models` for the speech models.

`docker compose run --rm models` fills that last volume. It runs
`scripts/setup-sussurro.sh` in its own image, which carries Python and the
converter for the translation models, so nothing gets installed on the
machine. It can be run again: whatever is already there is skipped. Until the
models are in place, the media engine stops at start-up with a message saying
which file is missing. To use a model directory that already exists on the
machine instead, set `AMBIVOX_MODELS=./models` in `.env`.

The images come from `ghcr.io/whispem/ambivox`, built for x86-64 and arm64 at
every release: `0.8` (and `latest`) for ambivox itself, `0.8-models` for the
model setup. `AMBIVOX_VERSION` picks another release. To build them from the
sources instead, add the second compose file:

~~~bash
docker compose -f compose.yaml -f compose.build.yaml run --rm --build models
docker compose -f compose.yaml -f compose.build.yaml up --build
~~~

The images run on any x86-64 CPU with AVX2, from 2013 onwards, and on any
arm64 CPU with ARMv8.2 dot product and half-precision instructions: Apple
silicon, Raspberry Pi 5, AWS Graviton. A build from the sources is tuned for
the machine that compiles it instead, which is a little faster there but may
not start elsewhere; set `AMBIVOX_PORTABLE=1` when building binaries to copy
to other machines. Each image holds the licences of what it was built from in
`/usr/share/licenses/ambivox`.

`AMBIVOX_ACCESS_KEY`, `AMBIVOX_INVITE_TTL`, `AMBIVOX_ECHO_GUARD`,
`AMBIVOX_PROFILE`, `AMBIVOX_WHISPER` and `AMBIVOX_TRANSLATOR` can go in `.env`
too; other settings from the table below are passed through `compose.yaml` the
same way.
`AMBIVOX_BACKEND=sim` runs the simulated backend, which needs no models.

### Without Docker

`make lan` starts both processes with the settings below. By hand:

~~~bash
ambivox-media serve --socket /tmp/ambivox-media.sock
ambivox -listen :8443 -tls-self-signed -udp-port 8443 -data-dir ~/.ambivox
~~~

The server prints the addresses to open, for instance:

~~~text
ambivox 0.8.0 is ready. Open this on any device on the same network:
  https://192.168.1.20:8443
  https://localhost:8443
The certificate is self-signed: each browser asks once to trust it. Its fingerprint is
  SHA-256 A8:50:05:60:…
~~~

It lists the IPv4 addresses of every interface that is up; `-host` replaces
that list with your own addresses or names. A firewall must let TCP and UDP
8443 in; macOS asks the first time.

### The certificate warning

Browsers only give a page the microphone over HTTPS, and no public authority
can vouch for an address on a private network, so ambivox signs its own
certificate. The first visit shows a warning: on Chrome choose *Advanced*,
then *Proceed*; on Safari, *Show Details*, then *visit this website*. To be
sure you are talking to your own server, compare the fingerprint the browser
shows with the one the server printed.

The certificate is valid for a little over a year and covers `localhost`, the
listed addresses and the `-host` names. It is renewed by itself when it nears
expiry or when the machine's address changes, which means one more warning:
giving the machine a fixed address in the router avoids that.

### Invitations and the access key

Starting a call creates a room with a random 128-bit name and an invitation
signed with HMAC-SHA256: the link carries the room, an expiry time and the
signature. A join without a valid, unexpired signature for that room is
refused. Links last 24 hours (`-invite-ttl`). The link puts both in the URL
fragment (`#room=…&t=…`), which browsers never send in HTTP requests, so it
does not end up in access logs.

The signing key is `invite.key` in the data directory. Deleting it and
restarting the server revokes every link handed out so far. Without a data
directory the key only lives in memory and links stop working when the server
restarts.

Anyone who can open the page can start a call. To restrict that, set
`AMBIVOX_ACCESS_KEY`: the page then asks for it before creating a room. Joining
with an invitation never needs it. The key is read from the environment only,
so it does not show up in the process list.

`-open-rooms` turns invitations off and goes back to rooms that anyone can join
by name, `https://host/#room=kitchen`.

## Privacy

Nothing is kept. There are no accounts and no database. Audio, transcripts and
translations exist in memory for as long as a sentence is being processed and
are never written to disk. Names and languages live as long as the call.

- **Logs** record that a participant joined or left a room, with random
  identifiers and the language, never a name, a caption or audio.
- **The data directory** holds the certificate, its private key and the
  invitation key. Nothing else.
- **Metrics** are counters and gauges with no labels.
- **The browser** stores nothing: no cookies, no local storage. The page loads
  no outside resources, and its Content-Security-Policy forbids them.
- **No network.** The models are downloaded once, at setup. After that the
  media engine and the server never open a connection beyond the machine;
  `make offline` runs complete calls in a network namespace that only has
  loopback, and fails if it finds any other interface.

The one exception is a STUN or TURN server you configure with `-ice-servers`:
browsers then contact it to find a route, and a TURN server relays the
encrypted audio.

## Behind your own reverse proxy

If ambivox should share a machine and a certificate with other services, let a
reverse proxy you already run terminate TLS and forward to ambivox in plain
HTTP on loopback:

~~~bash
ambivox -listen 127.0.0.1:8080 -udp-port 8443 -data-dir /var/lib/ambivox
~~~

The proxy has to forward WebSocket upgrades on `/ws`. With Caddy:

~~~text
ambivox.example.lan {
    reverse_proxy 127.0.0.1:8080
}
~~~

With nginx:

~~~nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 1h;
}
~~~

Audio does not go through the proxy: it is UDP straight between each browser
and ambivox, so open the `-udp-port` in the firewall. If browsers reach the
machine through an address it does not own itself (a NAT, a container), give
that address with `-public-ip`. The page and the WebSocket must keep the same origin;
otherwise list the page's origin in `-allow-origin`.

## Configuration

### ambivox (server)

Every flag has an environment variable; flags win.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-listen` | `AMBIVOX_LISTEN` | `:8080` | HTTP listen address |
| `-media-socket` | `AMBIVOX_MEDIA_SOCKET` | `/tmp/ambivox-media.sock` | media engine socket |
| `-max-rooms` | `AMBIVOX_MAX_ROOMS` | `100` | simultaneous rooms |
| `-ice-servers` | `AMBIVOX_ICE_SERVERS` | none | STUN/TURN URLs given to both ends |
| `-public-ip` | `AMBIVOX_PUBLIC_IP` | none | address to advertise behind 1:1 NAT |
| `-udp-port` | `AMBIVOX_UDP_PORT` | none | one UDP port shared by every call |
| `-udp-ports` | `AMBIVOX_UDP_PORTS` | any | UDP range for media, `MIN-MAX`, instead of `-udp-port` |
| `-ice-loopback` | `AMBIVOX_ICE_LOOPBACK=1` | off | offer loopback candidates |
| `-allow-origin` | `AMBIVOX_ALLOW_ORIGIN` | same origin | extra WebSocket origins |
| `-tls-cert`, `-tls-key` | `AMBIVOX_TLS_CERT`, `AMBIVOX_TLS_KEY` | none | serve HTTPS with your own certificate |
| `-tls-self-signed` | `AMBIVOX_TLS_SELF_SIGNED=1` | off | serve HTTPS with a generated certificate |
| `-host` | `AMBIVOX_HOST` | interface addresses | addresses or names put in the certificate and printed at start-up |
| `-data-dir` | `AMBIVOX_DATA_DIR` | none | keeps the certificate and invitation key across restarts |
| `-invite-ttl` | `AMBIVOX_INVITE_TTL` | `24h` | how long an invitation link works |
| `-open-rooms` | `AMBIVOX_OPEN_ROOMS=1` | off | rooms joinable by name, without invitations |
| | `AMBIVOX_ACCESS_KEY` | none | key required to start a call |
| `-echo-guard` | `AMBIVOX_ECHO_GUARD=0` disables | on | silence a speaker while their translation plays |
| `-log-level` | `AMBIVOX_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-log-format` | `AMBIVOX_LOG_FORMAT` | `text` | `text` or `json` |

### ambivox-media

| Flag | Default | Meaning |
|---|---|---|
| `--socket` | `/tmp/ambivox-media.sock` | socket to listen on (replaced if it exists) |
| `--backend` | `sim` | `sim` or `sussurro` |
| `--models` | none | model directory, required by `sussurro` |
| `--workers` | `2` | utterances interpreted in parallel |
| `--threads` | cores ÷ workers, at most 8 | threads each model may use |
| `--bitrate` | `32000` | Opus bitrate of translated speech |
| `--partials-ms` | `800` | how often live captions are refreshed while someone speaks; `0` turns them off |
| `--early-words` | `8` | in headphones mode, how many settled words are spoken before the sentence ends when no comma comes first |
| `--profile` | `balanced` | models to use: `light`, `balanced` or `quality` (see [Model directory](#model-directory)) |
| `--whisper` | from the profile | recognition model, `ggml-NAME.bin` in the model directory |
| `--translator` | from the profile | translation model: `opus` for the per-pair models, or `NAME.gguf` in the model directory, run with llama.cpp |

Budget roughly `workers × threads` cores for inference; by default the workers
share the cores. With two participants talking in turn, one worker is busy at a
time. On a CPU the thread count matters: whisper small took 1.57 s per
sentence with 4 threads and 0.97 s with 8.

### Model directory

```
models/
  ggml-small.bin                       whisper
  tc-en-fr.gguf  tc-en-es.gguf  tc-en-it.gguf
  tc-fr-en.gguf  tc-it-en.gguf  es-en.gguf
  tc-itc-itc.gguf                      French, Spanish and Italian among themselves
  vits-piper-en_US-ryan-medium/        one Piper voice per language
  vits-piper-fr_FR-tom-medium/
  vits-piper-es_ES-davefx-medium/
  vits-piper-it_IT-paola-medium/
```

`make models` builds this layout, through `scripts/setup-sussurro.sh`; in
Docker, `docker compose run --rm models` does the same. It is the layout
sussurro.cpp uses, plus the whisper model. Which models it holds depends on the
profile:

| Profile | Recognition | Translation | For |
|---|---|---|---|
| `light` | whisper `base` | opus | small or older machines; less accurate |
| `balanced` | whisper `small` | opus | any recent machine; the default |
| `quality` | whisper `large-v3-turbo` | TranslateGemma 12B | Apple silicon or a GPU |

`PROFILE=quality` picks one for `make models`, `make run` and `make bench`;
`AMBIVOX_PROFILE=quality` in `.env` does it for both the models step and the
engine in Docker. `WHISPER` and `TRANSLATOR` (`AMBIVOX_WHISPER` and
`AMBIVOX_TRANSLATOR` in Docker, `--whisper` and `--translator` for the engine)
replace either model of a profile. Any model published for whisper.cpp works;
`medium` sits between `small` and `large-v3-turbo`, and a larger one hears
accents, names and noisy rooms better.

Measured with `make bench` on an M5 Max, from the end of a sentence to the
translated voice, at the median:

| Profile | GPU (Metal) | CPU only (Docker, 8 threads) | Words misheard |
|---|---|---|---|
| `light` | 124 ms | 344 ms | 6 to 9% |
| `balanced` | 147 ms | 1.07 s | 2% |
| `quality` | 508 ms | too slow | 1% |

Without a GPU, `balanced` speaks about a second after each sentence; `light`
keeps up but mishears more, mostly in French and Italian. These figures come
from the synthetic voices; read aloud by a person, the French sentences were
misheard at 1.8% with `balanced` and 1.2% with `quality`.

The quality profile translates with TranslateGemma, Google's translation model
built on Gemma 3, run through llama.cpp. The setup downloads a `Q4_K_M`
quantisation of the 12B, about 7 GB, and checks it against a pinned checksum.
It is published under Google's Gemma terms, which you accept by downloading
it; ambivox itself stays MIT. One model covers every language pair, so this
profile skips the per-pair models. Another GGUF prompted the way TranslateGemma
expects can go in the model directory as `NAME.gguf`, picked with
`--translator NAME`.

On an M5 Max with the synthetic voices of `make bench`, the 12B scores a chrF
of 77.4 against 76.9 for opus on the sentence text, with clear gains from and
into Italian and Spanish and between French, Spanish and Italian, which opus
does not cover. It adds about 300 ms per sentence (421 ms total at the median
against 147 ms) and tends to use the formal register, `vous` in French. The
voice does not wait for the whole translation: each clause is spoken as soon as
the model has written it, so a sentence with a comma or a full stop in the
middle is heard sooner, 382 ms at the median on the bench and more on long
sentences. The 4B scored below opus and is not worth the extra time. On a CPU,
which includes the Docker images, the 12B is too slow for a call: keep
`balanced` there. Run `make bench` with each profile on your own machine before
choosing.

Two filters keep noises out of the captions. The voice detector only passes
sounds with a pitch, as voices have; a knock, a door or a clap has none and
never reaches whisper. Recognition then skips what whisper itself takes for
silence: a segment it believes holds no speech, and whose words it is unsure
of, is dropped rather than captioned and translated. Without them, whisper
tends to hear "thank you" or "merci" in any noise. Whispering has no pitch
either, so speak aloud.

### Measuring a model

`make bench BACKEND=sussurro` runs every sentence of `bench/sentences.txt`
through the engine, one after the other, and prints a table per language pair:

| Column | Meaning |
|---|---|
| `WER` | word error rate of recognition: words missed, added or misheard, over the words said |
| `chrF` | how close the translation of what was heard is to the reference translation, from 0 to 100 |
| `chrF text` | the same for a translation of the sentence as written, so translation alone, without recognition errors |
| `listen`, `translate`, `speak` | median time of each step |
| `voice p50` | time from the end of a sentence to the first words of the translated voice being ready; shorter than the total when a language model translates a sentence with a clause in it |
| `total p50`, `total p90` | time from the end of a sentence to the translated voice being ready, for half and nine in ten of the sentences |

Sentences recognition got wrong are listed below the table, with what was said
and what was heard. By default each sentence is spoken by the engine's own Piper
voice, which is clearer than a person in a room, so recognition looks better
than it is. For real figures, record yourself: `scripts/record-bench.sh fr`
shows each sentence of the set spoken in French, records it while you read it
aloud and saves it in `recordings/`, which git ignores. It needs sox
(`brew install sox` or `apt install sox`), skips what is already recorded so it
can be stopped and run again, and records once a sentence shared by several
pairs. Then `make bench BACKEND=sussurro BENCH_ARGS="--recordings recordings"`.
Any 16-bit mono WAV named after a sentence id works too; on a Mac, a voice memo
converts with `afconvert -f WAVE -d LEI16@16000 -c 1 memo.m4a fr-en-01.wav`.
Sentences without a recording keep the synthetic voice.

To compare models, run the same set with each and keep the results as JSON:

```
make bench BACKEND=sussurro BENCH_ARGS="--recordings ~/ambivox-voice --json small.json"
make bench BACKEND=sussurro WHISPER=large-v3-turbo BENCH_ARGS="--recordings ~/ambivox-voice --json turbo.json"
```

`--only fr-en` keeps one pair. The set is a plain text file with one sentence
per line and five fields separated by `|`: id, spoken language, target
language, sentence, reference translation. Recognition is compared in lower
case and without punctuation; a number counts the same spelled out or in
digits, up to 69, and an English contraction the same as its long form, so
"I've" and "I have" are not an error. chrF is compared in lower case. A
language pair whose models are missing is skipped and named above the table.

## Network

- Browsers only grant microphone access on `https://` or `localhost`. For
  anything beyond one machine, use `-tls-self-signed`, your own certificate
  with `-tls-cert`/`-tls-key`, or a reverse proxy.
- Signalling is HTTP and WebSocket on the listen port. Media is UDP directly
  between each browser and the server; open `-udp-port` (or the range given by
  `-udp-ports`).
- On a LAN no ICE server is needed. On a host with a public address behind 1:1
  NAT (most cloud instances) set `-public-ip`. Participants behind restrictive
  NATs need a TURN server listed in `-ice-servers`.
- The Unix socket carries unencrypted audio. Keep it on a filesystem only the
  two processes can reach.

## Endpoints

| Path | Purpose |
|---|---|
| `/` | web client |
| `/ws` | signalling WebSocket |
| `POST /rooms` | creates a room and its invitation |
| `/healthz` | liveness: `200` while the process serves HTTP |
| `/readyz` | readiness: `503` while the media engine is unreachable |
| `/config` | languages, backend name, engine link state and whether invitations and an access key are required, as JSON |
| `/metrics` | Prometheus text format |

## Metrics

Server:

| Metric | Type | Meaning |
|---|---|---|
| `ambivox_rooms`, `ambivox_peers` | gauge | current load |
| `ambivox_sessions_total` | counter | signalling sessions accepted |
| `ambivox_media_connected` | gauge | engine link up (1) or down (0) |
| `ambivox_media_frames_dropped` | gauge | frames dropped because the engine link was saturated |
| `ambivox_rtp_packets_received_total` | counter | packets from participants |
| `ambivox_rtp_packets_relayed_total` | counter | packets relayed untouched (same language) |
| `ambivox_rtp_packets_translated_total` | counter | packets sent to the engine |
| `ambivox_rtp_packets_muted_total` | counter | packets silenced by the echo guard |
| `ambivox_rtp_packets_same_room_muted_total` | counter | packets silenced because the other participant in the same room was speaking |
| `ambivox_speech_packets_sent_total` | counter | translated packets sent to participants |
| `ambivox_captions_total` | counter | final captions delivered |
| `ambivox_partial_captions_total` | counter | live caption updates delivered |

Engine counters arrive every two seconds and are re-exported with the prefix
`ambivox_engine_`: `streams`, `packets_in`, `packets_lost`, `packets_dropped`,
`utterances`, `partials`, `partials_skipped`, `frames_out`, `frames_dropped`,
`backlog_dropped_ms`, `errors`, `inference_ms`, `partial_ms`, `early_chunks`
(pieces of a sentence spoken before it ended, in headphones mode),
`streamed_clauses` (clauses of a translation spoken before a language model
finished it).

Worth alerting on: `ambivox_media_connected == 0`, a rising
`ambivox_engine_errors`, and `ambivox_engine_backlog_dropped_ms` above zero,
which means inference is slower than speech. `inference_ms / utterances` is the
average time to interpret one utterance.

## Troubleshooting

**"Translation engine offline" in the client.** The server cannot reach the
socket. Check that `ambivox-media` is running and that both sides use the same
path.

**Connected, but no audio and no captions.** Media is not arriving. Check that
UDP reaches `-udp-port` (or the range), and in Docker that `AMBIVOX_HOST` is
the address the other devices use. Across NAT, set `-public-ip` or use a TURN
server. `-log-level debug` shows ICE state changes.

**The same sentence comes back again and again, more garbled each time.** Two
devices in one room, without headphones: one plays the translation, the other
hears it and sends it to be translated again. The echo guard stops this; make
sure it has not been turned off with `-echo-guard=false`.

**Words nobody said, with both devices in one room.** Each microphone also
hears the other person, so their voice is sent twice, once as if it were
yours. Press **Same room** on either device. If
`ambivox_rtp_packets_same_room_muted_total` stays at zero with the mode on,
the browser does not report audio levels and the mode cannot work there.

**The beginning of a sentence is lost.** It was said while the previous
translation was still playing, so the echo guard silenced it. Wait for the
translation to finish, or, with headphones on both sides, press
**Headphones**.

**"This invitation link has expired" or "is not valid".** Start a new call and
send the new link. Links also stop working when the invitation key changes:
after deleting it, or after a restart without a data directory.

**The phone shows a certificate warning again.** The machine's address
changed, so the certificate was renewed. Give it a fixed address in the
router.

**Captions appear late.** Look at `ambivox_engine_inference_ms` against
`ambivox_engine_utterances`. On a CPU, recognition takes most of that time:
raise `--threads` if cores sit idle, or switch to the `light` profile.

**Live captions lag or come in bursts.** Each draft re-runs recognition on the
sentence so far. `partial_ms / partials` is the cost of one draft; if it is
close to `--partials-ms`, raise the interval, or set it to `0` on a machine that
struggles: the final captions and the voice are unaffected either way.

**Headphones mode speaks no earlier.** It needs live captions: with
`--partials-ms 0` there is nothing to settle on. If `ambivox_engine_early_chunks`
stays at zero, the drafts keep changing their first words; a quieter room or a
larger whisper model steadies them. Lower `--early-words` to start sooner, at
the cost of translating smaller pieces.

**Words are cut or sentences merge.** The voice detector is energy-based; a
headset helps more than any setting.

**Building with CMake 4.** The bundled Opus sources declare an old minimum
CMake version. The Makefile and `.cargo/config.toml` set
`CMAKE_POLICY_VERSION_MINIMUM=3.5` for that reason; export it yourself if you
call CMake-driven builds some other way.
