#!/usr/bin/env bash
# Local fake gpac for acceptance/E2E verification (POSIX/bash).
#
# Simulates the three observable gpac behaviors without needing media:
#   FAKE_GPAC_MODE=success (default) -> writes <out>/index.m3u8 + seg0.ts, exit 0
#   FAKE_GPAC_MODE=fail              -> prints an error and exits 7
#   FAKE_GPAC_MODE=slow              -> writes a playlist, then blocks until killed
#
# Optional:
#   FAKE_GPAC_CALLS  file to append one line per invocation (assert "not called")
#   FAKE_GPAC_READY  file to create once slow mode is running
#
# It understands the single "-o <path>/index.m3u8:segdur=..." argument emitted by
# GPACCommand. To use it on Linux/macOS:
#   cp scripts/fake-gpac/gpac.sh /tmp/fakebin/gpac && chmod +x /tmp/fakebin/gpac
#   PATH=/tmp/fakebin:$PATH go test ./...
# or point GPAC_BIN at the script.
set -u

mode="${FAKE_GPAC_MODE:-success}"

if [ -n "${FAKE_GPAC_CALLS:-}" ]; then
  printf 'call\n' >> "$FAKE_GPAC_CALLS"
fi

playlist=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then
    # Strip the ":segdur=..." option suffix.
    playlist="${a%%:segdur=*}"
  fi
  prev="$a"
done

write_playlist() {
  [ -z "$playlist" ] && return 0
  mkdir -p "$(dirname "$playlist")"
  printf '#EXTM3U\n# fake gpac\n' > "$playlist"
  printf 'fake-segment' > "$(dirname "$playlist")/seg0.ts"
}

case "$mode" in
  fail)
    echo "fake gpac failure" >&2
    exit 7
    ;;
  slow)
    write_playlist
    [ -n "${FAKE_GPAC_READY:-}" ] && echo ready > "$FAKE_GPAC_READY"
    # Block until terminated.
    while :; do sleep 1; done
    ;;
  *)
    write_playlist
    exit 0
    ;;
esac
