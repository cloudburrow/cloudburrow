#!/usr/bin/env bash
# build-macos.sh: build cloudburrow_generate for macOS arm64 (#2159), the
# same runner as the Linux image (Dockerfile beside this script), from the
# same LiteRT-LM commit with the same patches, so Gemini generation can run
# on the Apple GPU (LiteRT-LM's --backend=gpu, WebGPU on Dawn's Metal
# backend). A Linux container cannot reach Metal, hence a native build.
#
# Usage: deploy/litert-lm/build-macos.sh <work-dir> <out.tar.gz>
#
# Requires macOS on Apple silicon with Xcode (its clang and SDK), git and
# git-lfs. Everything else is fetched pinned: bazelisk by SHA-256 (the
# dependencies.json localai.bazelisk sum), which then runs the checkout's
# .bazelversion (7.6.1); LiteRT-LM by commit (localai.litertLM.commit).
# Bazel's output tree is kept under <work-dir>, never ~/.cache or
# /private/var/tmp, and removed at the end unless KEEP_BAZEL=1.
#
# The archive holds cloudburrow_generate and the prebuilt dylibs it links
# through @rpath (upstream's prebuilt/macos_arm64, found beside the binary
# through @loader_path), exactly as upstream's own CI packages
# litert_lm_main for its macOS release. TestMacOSBuildPinsTheInventory
# (internal/hostai) keeps the pins here equal to the Dockerfile's.
set -euo pipefail

work=${1:?usage: build-macos.sh <work-dir> <out.tar.gz>}
out=${2:?usage: build-macos.sh <work-dir> <out.tar.gz>}
here=$(cd "$(dirname "$0")" && pwd)

[ "$(uname -s)-$(uname -m)" = Darwin-arm64 ] || { echo "build-macos.sh: needs macOS on Apple silicon" >&2; exit 1; }

LITERT_LM_REF=0082833dc28014489ceaa33eacaf5edbffbae518
BAZELISK_VERSION=v1.25.0
BAZELISK_SHA256=b13dd89c6ecd90944ca3539f5a2c715a18f69b7458878c471a902a8e482ceb4b

mkdir -p "$work" "$(dirname "$out")"
work=$(cd "$work" && pwd)
out="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
bazel="$work/bazelisk"
if [ ! -x "$bazel" ] || [ "$(shasum -a 256 "$bazel" | cut -d' ' -f1)" != "$BAZELISK_SHA256" ]; then
  curl -fsSL -o "$bazel.part" "https://github.com/bazelbuild/bazelisk/releases/download/$BAZELISK_VERSION/bazelisk-darwin-arm64"
  echo "$BAZELISK_SHA256  $bazel.part" | shasum -a 256 -c -
  chmod +x "$bazel.part"
  mv "$bazel.part" "$bazel"
fi

src="$work/src"
if [ ! -d "$src/.git" ]; then
  git lfs install --skip-repo
  git clone https://github.com/google-ai-edge/LiteRT-LM.git "$src"
fi
cd "$src"
git checkout -q --force "$LITERT_LM_REF"
git clean -fdq
git lfs pull
for p in "$here"/patches/*.patch; do git apply --verbose "$p"; done
mkdir -p cloudburrow
cp "$here"/runner/* cloudburrow/
cp runtime/engine/litert_lm_main.exported_symbols cloudburrow/

root="$work/bazel-root"
"$bazel" --output_user_root="$root" build --config=macos_arm64 \
  --define=LITERT_LM_FST_CONSTRAINTS_DISABLED=1 \
  //cloudburrow:cloudburrow_generate

stage="$work/stage"
rm -rf "$stage"
mkdir -p "$stage"
cp bazel-bin/cloudburrow/cloudburrow_generate "$stage/"
chmod u+w "$stage/cloudburrow_generate"
for lib in $(otool -L bazel-bin/cloudburrow/cloudburrow_generate | awk '$1 ~ /^@rpath\// { sub(/^@rpath\//, "", $1); print $1 }'); do
  cp "prebuilt/macos_arm64/$lib" "$stage/"
done
# The GPU backend loads Dawn on demand rather than at link time; it goes
# too, as upstream's README has every prebuilt beside the binary for GPU.
cp prebuilt/macos_arm64/*.dylib "$stage/"
# The licences of what the archive carries: LiteRT-LM (Apache-2.0, the
# runner and libGemmaModelConstraintProvider), Dawn (BSD-3-Clause,
# libwebgpu_dawn) and llguidance (MIT, linked into the runner).
ext="$("$bazel" --output_user_root="$root" info output_base)/external"
cp LICENSE "$stage/LICENSE-LiteRT-LM"
cp "$ext/dawn/LICENSE" "$stage/LICENSE-Dawn"
cp "$ext"/crate_index__llguidance-*/LICENSE "$stage/LICENSE-llguidance"
# dyld stops before main() when a dylib is missing; --version exits 0.
"$stage/cloudburrow_generate" --version >/dev/null
tar -czf "$out" -C "$stage" .
shasum -a 256 "$out" "$stage"/*
if [ "${KEEP_BAZEL:-}" != 1 ]; then
  "$bazel" --output_user_root="$root" clean --expunge
  chmod -R u+w "$root"
  rm -rf "$root"
fi
