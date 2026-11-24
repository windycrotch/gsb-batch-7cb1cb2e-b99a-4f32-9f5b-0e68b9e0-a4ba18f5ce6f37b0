#!/usr/bin/env bash
# Fetches everything the real speech backend needs:
#   third_party/sherpa-onnx   prebuilt text-to-speech runtime
#   models/                   whisper, translation and Piper voice models
#
# usage: scripts/setup-sussurro.sh [sherpa] [voices] [whisper] [translation] [converter]
# With no argument every step but converter runs, which translation runs when
# it needs to. Steps are idempotent. AMBIVOX_CONVERT_PYTHON names a Python that
# already has the converter's requirements, instead of a private venv.
# AMBIVOX_PROFILE picks the models (light, balanced or quality; balanced by
# default); AMBIVOX_WHISPER and AMBIVOX_TRANSLATOR override either.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODELS="${AMBIVOX_MODELS:-$ROOT/models}"
THIRD_PARTY="$ROOT/third_party"
SHERPA_VERSION="v1.13.8"
SUSSURRO_REV="559bdbb6124e7b9f8ed63fb15e00bca33c66c4d9"
VOICES=(en_US-ryan-medium fr_FR-tom-medium es_ES-davefx-medium it_IT-paola-medium)
TRANSLATION_MODELS=(
  "Helsinki-NLP/opus-mt-tc-big-itc-itc tc-itc-itc"
  "Helsinki-NLP/opus-mt-tc-big-en-fr tc-en-fr"
  "Helsinki-NLP/opus-mt-tc-big-en-es tc-en-es"
  "Helsinki-NLP/opus-mt-tc-big-en-it tc-en-it"
  "Helsinki-NLP/opus-mt-tc-big-fr-en tc-fr-en"
  "Helsinki-NLP/opus-mt-tc-big-it-en tc-it-en"
  "Helsinki-NLP/opus-mt-es-en es-en"
)
LANGUAGE_MODELS=(
  "translategemma-12b https://huggingface.co/anilb/translategemma-gguf/resolve/main/translategemma-12b-it.Q4_K_M.gguf b7aac4b4be7ab0c49b6556c29c4467e74313df7f1e95d9f9676bb2adf0afa528 https://ai.google.dev/gemma/terms"
)

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

PROFILE="${AMBIVOX_PROFILE:-balanced}"
case "$PROFILE" in
  light) WHISPER=base TRANSLATOR=opus ;;
  balanced) WHISPER=small TRANSLATOR=opus ;;
  quality) WHISPER=large-v3-turbo TRANSLATOR=translategemma-12b ;;
  *) die "unknown profile: $PROFILE (expected light, balanced or quality)" ;;
esac
WHISPER="${AMBIVOX_WHISPER:-$WHISPER}"
TRANSLATOR="${AMBIVOX_TRANSLATOR:-$TRANSLATOR}"

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

setup_sherpa() {
  local dest="$THIRD_PARTY/sherpa-onnx"
  if [ -f "$dest/include/sherpa-onnx/c-api/c-api.h" ]; then
    say "sherpa-onnx already present"
    return
  fi
  local platform
  case "$(uname -s)-$(uname -m)" in
    Darwin-arm64)  platform="osx-arm64-shared" ;;
    Darwin-x86_64) platform="osx-x64-shared" ;;
    Linux-x86_64)  platform="linux-x64-shared" ;;
    Linux-aarch64) platform="linux-aarch64-shared-cpu" ;;
    *) die "no prebuilt sherpa-onnx for $(uname -s)-$(uname -m); build it and set AMBIVOX_SHERPA_DIR" ;;
  esac
  local name="sherpa-onnx-$SHERPA_VERSION-$platform"
  say "downloading $name"
  mkdir -p "$THIRD_PARTY"
  curl -fSL "https://github.com/k2-fsa/sherpa-onnx/releases/download/$SHERPA_VERSION/$name.tar.bz2" \
    | tar xj -C "$THIRD_PARTY"
  mv "$THIRD_PARTY/$name" "$dest"
  if [ ! -f "$dest/include/sherpa-onnx/c-api/c-api.h" ]; then
    mkdir -p "$dest/include/sherpa-onnx/c-api"
    curl -fsSL -o "$dest/include/sherpa-onnx/c-api/c-api.h" \
      "https://raw.githubusercontent.com/k2-fsa/sherpa-onnx/$SHERPA_VERSION/sherpa-onnx/c-api/c-api.h"
  fi
  curl -fsSL -o "$dest/LICENSE" "https://raw.githubusercontent.com/k2-fsa/sherpa-onnx/$SHERPA_VERSION/LICENSE"
  if [ "$(uname -s)" = "Darwin" ]; then
    xattr -dr com.apple.quarantine "$dest" 2>/dev/null || true
  fi
}

setup_voices() {
  mkdir -p "$MODELS"
  for voice in "${VOICES[@]}"; do
    if [ -d "$MODELS/vits-piper-$voice" ]; then
      say "voice $voice already present"
      continue
    fi
    say "downloading voice $voice"
    curl -fSL "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-$voice.tar.bz2" \
      | tar xj -C "$MODELS"
  done
}

setup_whisper() {
  local name="$WHISPER"
  [[ "$name" =~ ^[A-Za-z0-9_-][A-Za-z0-9._-]*$ ]] || die "invalid whisper model name: $name"
  mkdir -p "$MODELS"
  if [ -f "$MODELS/ggml-$name.bin" ]; then
    say "whisper $name already present"
    return
  fi
  say "downloading whisper $name"
  curl -fSL -o "$MODELS/ggml-$name.bin.part" \
    "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-$name.bin"
  mv "$MODELS/ggml-$name.bin.part" "$MODELS/ggml-$name.bin"
}

setup_translation() {
  if [ "$TRANSLATOR" != opus ]; then
    setup_language_model
    return
  fi
  mkdir -p "$MODELS"
  local missing=()
  for entry in "${TRANSLATION_MODELS[@]}"; do
    read -r _ name <<<"$entry"
    [ -f "$MODELS/$name.gguf" ] || missing+=("$entry")
  done
  if [ ${#missing[@]} -eq 0 ]; then
    say "translation models already present"
    return
  fi

  setup_converter
  for entry in "${missing[@]}"; do
    read -r repo name <<<"$entry"
    say "converting $repo"
    "$PYTHON" "$CONVERTER/scripts/convert.py" --model "$repo" --outfile "$MODELS/$name.gguf.part"
    mv "$MODELS/$name.gguf.part" "$MODELS/$name.gguf"
  done
}

setup_language_model() {
  [[ "$TRANSLATOR" =~ ^[A-Za-z0-9_-][A-Za-z0-9._-]*$ ]] || die "invalid translation model name: $TRANSLATOR"
  mkdir -p "$MODELS"
  if [ -f "$MODELS/$TRANSLATOR.gguf" ]; then
    say "$TRANSLATOR already present"
    return
  fi
  local entry name url sum terms
  for entry in "${LANGUAGE_MODELS[@]}"; do
    read -r name url sum terms <<<"$entry"
    [ "$name" = "$TRANSLATOR" ] && break
    url=""
  done
  [ -n "$url" ] || die "no download known for $TRANSLATOR: put $TRANSLATOR.gguf in $MODELS"
  say "downloading $TRANSLATOR, published under $terms, which you accept by downloading it"
  curl -fSL -C - -o "$MODELS/$TRANSLATOR.gguf.part" "$url"
  [ "$(sha256 "$MODELS/$TRANSLATOR.gguf.part")" = "$sum" ] || die "$TRANSLATOR does not match its checksum; delete $MODELS/$TRANSLATOR.gguf.part and retry"
  mv "$MODELS/$TRANSLATOR.gguf.part" "$MODELS/$TRANSLATOR.gguf"
}

setup_converter() {
  CONVERTER="$THIRD_PARTY/sussurro.cpp"
  if [ ! -f "$CONVERTER/scripts/convert.py" ]; then
    say "fetching the sussurro.cpp converter"
    mkdir -p "$THIRD_PARTY"
    git clone --quiet https://github.com/whispem/sussurro.cpp "$CONVERTER"
    git -C "$CONVERTER" checkout --quiet "$SUSSURRO_REV"
  fi
  if [ -n "${AMBIVOX_CONVERT_PYTHON:-}" ]; then
    PYTHON="$AMBIVOX_CONVERT_PYTHON"
    return
  fi
  local venv="$THIRD_PARTY/convert-venv"
  if [ ! -x "$venv/bin/python" ]; then
    say "creating the conversion environment"
    python3 -m venv "$venv"
    "$venv/bin/pip" install --quiet --upgrade pip
    "$venv/bin/pip" install --quiet -r "$CONVERTER/requirements.txt"
  fi
  PYTHON="$venv/bin/python"
}

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(sherpa voices whisper translation)
for step in "${steps[@]}"; do
  case "$step" in
    sherpa|voices|whisper|translation|converter) "setup_$step" ;;
    *) die "unknown step: $step (expected sherpa, voices, whisper, translation or converter)" ;;
  esac
done

say "done: models are in $MODELS"
