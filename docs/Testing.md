# Testing

## Running the test suite

```bash
# all packages
go test ./...

# the concurrency-sensitive packages, with the race detector
go test -race -count=1 ./relay/ ./srt/ ./mpegts/
```

`make test` runs the full suite. `make test-race` runs the packages that cover
the relay lifecycle under the race detector. The realtime SRT tests bind
loopback UDP ports; skip them with `-short` if the sandbox has no network.

The tests are deterministic and self-contained: they do not need ffmpeg, ffprobe
or any network access. The ffprobe cross-check in `mpegts` is skipped
automatically when `ffprobe` is not in `PATH`.

## Relay lifecycle coverage

The stream lifecycle is covered at three levels.

`relay/relay_concurrency_test.go` and `relay/channel_lifecycle_test.go` cover:

- **publisher disconnect vs. subscriber read** – `TestRelay_CloseWhileReading`
  and `TestRelay_UnsubscribeWhilePublishing` race `Publisher.Close` and
  `Unsubscribe` against readers that hold and release buffers. They assert that
  readers terminate, no buffer is released twice, and every pool buffer is
  returned (`Relay.BuffersInUse()` reaches zero).
- **rapid republish of the same name** – `TestRelay_RapidRepublishConverges`
  publishes and closes the same name 400 times while subscribers and statistics
  readers run in parallel, then asserts that the stream list is empty and the
  client count is zero.
- **buffer return on stream close** – `TestChannel_CloseReturnsQueuedBuffers`
  queues buffers that no reader consumes, closes the channel, and asserts that
  `BufferPool.InUse()` is zero afterwards.
- **epoch isolation** – `TestRelay_NewEpochNeverLeaksOldData` and
  `TestRelay_ConcurrentTakeoverMissesNoData` verify that a subscriber of the new
  publisher never receives data written by the previous one.

`srt/server_lifecycle_test.go` drives the real `Server.Handle` path with an
in-memory `gosrt.Conn` (`fakeConn`, which implements the full interface), so the
publisher loop, the play loop, the statistics API and the connection teardown
are exercised end to end. It covers publisher disconnect while subscribers read,
immediate republish, duplicate publisher rejection, statistics, and the normal
push/pull path.

`srt/realtime_test.go` repeats the critical scenarios over real SRT sockets on a
loopback port, i.e. through the actual protocol stack rather than the in-memory
connection: republish after disconnect, repeated publish/pull churn, and
duplicate publisher handling.

```bash
go test -run 'TestRealtime' -v ./srt/
```

## Verifying with locally constructed MPEG-TS data

There are two automated ways plus one manual walkthrough; none of the automated
ones requires sample media or external tools.

1. **Synthetic packets (no tools at all).** `mpegts/synthetic_test.go` builds a
   PAT, a PMT and a PES packet containing an H.264 SPS in plain Go, feeds them
   to the parser and asserts that a complete decoder init is produced. It also
   asserts that the parser never hands out slices of caller memory, which is
   what makes it safe to parse pooled relay buffers.

   ```bash
   go test -run 'TestParser_Synthetic|TestParser_InitDataDoesNotAliasInput' -v ./mpegts/
   ```

2. **Relay the checked-in MPEG-TS sample.** `srt/server_lifecycle_test.go`
   relays `mpegts/h264.ts` (33 packets, one second of H.264) through the full
   server:

   - `TestServer_PublishPlayMpegTs` pushes the sample through the publisher loop
     and asserts that the subscriber receives exactly the same bytes and that
     the result still parses as MPEG-TS with decoder init data.
   - `TestServer_SyncClientsWithMpegTs` starts a subscriber after the stream
     began (with `syncClients` enabled) and asserts that it starts at a decoder
     synchronization point and receives a valid, parseable stream.

   ```bash
   go test -run 'TestServer_.*MpegTs' -v ./srt/
   ```

3. **Manually, with ffmpeg/ffplay** against a local relay build:
   ```bash
   go build -o srtrelay .
   ./srtrelay -addresses 127.0.0.1:1337
   # publish the checked-in sample
   ffmpeg -re -i mpegts/h264.ts -c copy -f mpegts 'srt://127.0.0.1:1337?streamid=publish/demo'
   # pull it in another terminal
   ffplay -fflags nobuffer 'srt://127.0.0.1:1337?streamid=play/demo'
   # inspect the reported streams (API must be enabled in the config)
   curl -s http://127.0.0.1:8080/streams
   ```
   To check the reconnect behaviour manually, stop the publisher and start it
   again without waiting: the new publisher claims `demo` immediately, and the
   old player disconnects instead of hanging. `curl` the `/streams` endpoint in
   between to see the stream disappear and come back with the correct client
   count.
