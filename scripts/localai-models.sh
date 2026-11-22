#!/usr/bin/env bash
# localai-models.sh: fetch what the localai compat shard's instance needs
# beyond the CLI (#1985), each pinned in dependencies.json and checked by
# SHA-256 before it is used:
#   - the generation model, localai.generationModel: the artifact at the
#     pinned Hugging Face revision (public and ungated: no sign-in, no token);
#   - on Linux amd64, ONNX Runtime's shared library, localai.onnxRuntime,
#     from Microsoft's PyPI wheel (elsewhere, set ONNXRUNTIME_LIB yourself,
#     e.g. Homebrew's onnxruntime on macOS).
# A file already in <dir> that matches its pin is not fetched again, so
# ci.yml restores <dir> from a cache keyed by the pins and calls this to
# check it. The embedding models, whisper.cpp's, Tesseract's and MediaPipe's
# are fetched by the instance itself into <state-dir>/models, each by its
# pin in internal/localai.
#
# Usage: scripts/localai-models.sh [--onnx-runtime-only] <dir>
# Prints CLOUDBURROW_LOCALAI_MODEL=<path> and, on Linux amd64,
# ONNXRUNTIME_LIB=<path>, for $GITHUB_ENV or eval. --onnx-runtime-only
# fetches ONNX Runtime alone, without the generation model (onnx.yml's
# `make onnx-test`). Requires curl, jq, unzip on Linux, and sha256sum or
# shasum.
set -euo pipefail

runtime_only=
if [ "${1:-}" = --onnx-runtime-only ]; then
  runtime_only=1
  shift
fi
dir=${1:?usage: scripts/localai-models.sh [--onnx-runtime-only] <dir>}
root=$(cd "$(dirname "$0")/.." && pwd)
deps="$root/dependencies.json"
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetch <url> <dest> <sha256> <bytes>: <dest> verified, fetched only if it is
# missing or does not match.
fetch() {
  local url=$1 dest=$2 sum=$3 bytes=$4
  if [ -f "$dest" ] && [ "$(sha256 "$dest")" = "$sum" ]; then
    echo "localai-models: $dest matches its pin" >&2
    return
  fi
  echo "localai-models: fetching $url ($bytes bytes)" >&2
  curl --retry 5 --retry-all-errors -fsSL -o "$dest.part" "$url"
  local got
  got=$(wc -c < "$dest.part" | tr -d ' ')
  [ "$got" = "$bytes" ] || { echo "localai-models: $url is $got bytes, want $bytes" >&2; rm -f "$dest.part"; exit 1; }
  got=$(sha256 "$dest.part")
  [ "$got" = "$sum" ] || { echo "localai-models: $url has SHA-256 $got, want $sum" >&2; rm -f "$dest.part"; exit 1; }
  mv "$dest.part" "$dest"
}

pin() { jq -er ".components.localai.$1" "$deps"; }

if [ -z "$runtime_only" ]; then
  repo=$(pin generationModel.repo)
  artifact=$(pin generationModel.artifact)
  rev=$(pin generationModel.revision)
  model="$dir/$artifact"
  fetch "https://huggingface.co/$repo/resolve/$rev/$artifact" "$model" "$(pin generationModel.sha256)" "$(pin generationModel.sizeBytes)"
  echo "CLOUDBURROW_LOCALAI_MODEL=$model"
fi

if [ "$(uname -s)-$(uname -m)" = Linux-x86_64 ]; then
  ver=$(pin onnxRuntime.version)
  wheel="$dir/onnxruntime-$ver.whl"
  fetch "$(pin '"onnxRuntime".artifacts."linux/amd64".url')" "$wheel" \
    "$(pin '"onnxRuntime".artifacts."linux/amd64".sha256')" "$(pin '"onnxRuntime".artifacts."linux/amd64".sizeBytes')"
  lib="$dir/onnxruntime-$ver/libonnxruntime.so.$ver"
  if [ ! -f "$lib" ]; then
    mkdir -p "$dir/onnxruntime-$ver"
    # Only the library, flattened: nothing else of the wheel is needed.
    unzip -q -o -j "$wheel" "onnxruntime/capi/libonnxruntime.so.$ver" -d "$dir/onnxruntime-$ver"
  fi
  echo "ONNXRUNTIME_LIB=$lib"
fi
