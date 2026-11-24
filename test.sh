#!/usr/bin/env bash
# Runs the local test suite for the re-publish takeover fixes.
#
# The project uses the GOPATH layout (src/vendor), not Go modules, so tests must
# run with GO111MODULE=off and GOPATH set to the repository root.
set -euo pipefail

cd "$(dirname "$0")"
REPO="$(pwd)"

export GOPATH="$REPO"
export GO111MODULE=off

PKGS="./src/vendor/sheepbao.com/media/protocol/rtmp/ \
      ./src/vendor/sheepbao.com/media/protocol/hls/ \
      ./src/vendor/sheepbao.com/media/utils/cmap/"

echo "== go vet =="
go vet \
  ./src/vendor/sheepbao.com/media/protocol/rtmp/ \
  ./src/vendor/sheepbao.com/media/protocol/hls/ \
  ./src/vendor/sheepbao.com/media/protocol/httpflv/ \
  ./src/vendor/sheepbao.com/media/utils/cmap/ \
  ./src/vendor/sheepbao.com/media/av/

echo "== go test (race) =="
go test -race -count=1 $PKGS
