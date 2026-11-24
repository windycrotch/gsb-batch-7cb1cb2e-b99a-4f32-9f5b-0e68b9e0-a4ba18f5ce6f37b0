#!/usr/bin/env bash
# Gathers the licences of everything compiled into the image, so the image
# carries them alongside the binaries.
#
# usage: scripts/collect-licenses.sh media|server DEST
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
part="$1"
dest="$2"

keep() {
  local name="$1"
  shift
  for file in "$@"; do
    [ -f "$file" ] || continue
    mkdir -p "$dest/$name"
    cp "$file" "$dest/$name/"
  done
}

case "$part" in
  media)
    keep ambivox "$ROOT/LICENSE"
    registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
    for crate in opus opusic-sys; do
      version="$(grep -A1 "^name = \"$crate\"$" "$ROOT/Cargo.lock" | sed -n 's/^version = "\(.*\)"/\1/p' || true)"
      keep "$crate" "$registry"/*/"$crate-$version"/LICENSE* "$registry"/*/"$crate-$version"/COPYING*
    done
    keep libopus "$registry"/*/opusic-sys-*/opus/COPYING
    for file in "$ROOT"/target/release/build/ambivox-engine{-*,/*}/out/build/_deps/*-src/LICENSE \
                "$ROOT"/target/release/build/ambivox-engine{-*,/*}/out/build/_deps/*-src/third_party/*/LICENSE; do
      [ -f "$file" ] || continue
      name="$(basename "$(dirname "$file")")"
      keep "${name%-src}" "$file"
    done
    if [ -f "$ROOT/third_party/sherpa-onnx/LICENSE" ]; then
      keep sherpa-onnx "$ROOT/third_party/sherpa-onnx/LICENSE"
      keep onnxruntime "$ROOT/scripts/licenses/onnxruntime.txt"
    fi
    ;;
  server)
    cd "$ROOT/server"
    keep go "$(go env GOROOT)/LICENSE"
    go list -deps -f '{{with .Module}}{{.Path}} {{.Dir}}{{end}}' ./cmd/ambivox | sort -u |
      while read -r path dir; do
        [ -z "$dir" ] || keep "$path" "$dir"/LICENSE* "$dir"/COPYING*
      done
    ;;
  *)
    echo "usage: $0 media|server DEST" >&2
    exit 2
    ;;
esac
