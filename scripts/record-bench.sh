#!/usr/bin/env bash
# Records your voice for the sentences of a bench set, one 16 kHz mono WAV per
# sentence, named after its id, for `ambivox-media bench --recordings DIR`.
#
# usage: scripts/record-bench.sh LANG [DIR] [SET]
# LANG is the spoken language (en, fr, es or it). DIR defaults to recordings/,
# SET to bench/sentences.txt. Sentences already recorded are skipped, so the
# script can be stopped and run again; delete a file to record it again. A
# sentence that appears in several pairs is recorded once. Needs sox.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
lang="${1:?usage: scripts/record-bench.sh LANG [DIR] [SET]}"
dir="${2:-$ROOT/recordings}"
set_file="${3:-$ROOT/bench/sentences.txt}"

command -v rec >/dev/null || {
  echo "error: sox is needed to record (brew install sox, or apt install sox)" >&2
  exit 1
}
[ -f "$set_file" ] || { echo "error: no set at $set_file" >&2; exit 1; }
mkdir -p "$dir"

trim() { sed 's/^[[:space:]]*//; s/[[:space:]]*$//' <<<"$1"; }

recorded_as() {
  local text="$1" id src tgt sentence rest
  while IFS='|' read -r id src tgt sentence rest; do
    [[ "$id" =~ ^[[:space:]]*# ]] && continue
    id="$(trim "$id")"
    [ "$(trim "$sentence")" = "$text" ] && [ -f "$dir/$id.wav" ] && { echo "$dir/$id.wav"; return 0; }
  done <"$set_file"
  return 0
}

total=0
done_now=0
while IFS='|' read -r id src tgt sentence rest <&3; do
  [[ "$id" =~ ^[[:space:]]*(#|$) ]] && continue
  [ "$(trim "$src")" = "$lang" ] || continue
  id="$(trim "$id")"
  sentence="$(trim "$sentence")"
  total=$((total + 1))
  out="$dir/$id.wav"
  [ -f "$out" ] && continue

  same="$(recorded_as "$sentence")"
  if [ -n "$same" ]; then
    cp "$same" "$out"
    echo "$id: same sentence as $(basename "$same"), copied"
    continue
  fi

  printf '\n%s\n\n    %s\n\n' "$id" "$sentence"
  read -r -p "Press Enter, read the sentence aloud, then press Enter again " </dev/tty
  rec -V1 -q -c 1 -r 16000 -b 16 "$out" &
  pid=$!
  read -r </dev/tty
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" || true
  if [ -s "$out" ]; then
    done_now=$((done_now + 1))
  else
    rm -f "$out"
    echo "$id: nothing was recorded; run the script again to retry it"
  fi
done 3<"$set_file"

[ "$total" -gt 0 ] || { echo "error: no sentence spoken in $lang in $set_file" >&2; exit 1; }
echo
echo "$done_now recorded now, in $dir."
echo "Run: make bench BACKEND=sussurro BENCH_ARGS=\"--recordings $dir\""
