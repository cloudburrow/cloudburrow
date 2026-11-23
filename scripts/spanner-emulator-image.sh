#!/usr/bin/env bash
# Build CloudBurrow's patched Cloud Spanner emulator image (#2029).
#
# Google's emulator (Apache-2.0) at the tag third_party/spanner-emulator/
# sources.json pins, its commit checked, with CloudBurrow's patches applied in
# sources.json order, built with the emulator's own Dockerfile up to its
# Bazel step and third_party/spanner-emulator/Dockerfile.tail after it, whose
# Bazel output base is a BuildKit cache mount: GoogleSQL takes about an hour
# or more on 8 CPUs, and an interrupted build keeps what it compiled.
#
# Usage: scripts/spanner-emulator-image.sh [-platform linux/arm64] [-check]
#   -check  only clone, verify and apply the patches; build nothing.
# Env: SPANNER_EMULATOR_BUILD_DIR (default third_party/spanner-emulator/build),
#      BAZEL_JOBS (default 4: each GoogleSQL compiler can take gigabytes).
# Prints the image it built, internal/spannerimage.Ref:
# dev.local/cloudburrow-spanner-emulator:<version>-cb.<patch hash>.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
dir="$root/third_party/spanner-emulator"
src_json="$dir/sources.json"
build="${SPANNER_EMULATOR_BUILD_DIR:-$dir/build}"
platform=""
check=0
while [ $# -gt 0 ]; do
  case "$1" in
    -platform) platform="$2"; shift 2 ;;
    -check) check=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

repo=$(jq -r .emulator.repository "$src_json")
tag=$(jq -r .emulator.tag "$src_json")
commit=$(jq -r .emulator.commit "$src_json")
dockerfile=$(jq -r .emulator.dockerfile "$src_json")
bazel_patch=$(jq -r .googlesql.bazelPatch "$src_json")

# The image tag names the upstream tag and a hash of every patch, in order.
hash=$( { jq -r '.emulator.patches[], .googlesql.patches[]' "$src_json" | while read -r p; do
  cat "$dir/$p"; done; } | shasum -a 256 | cut -c1-12)
image="dev.local/cloudburrow-spanner-emulator:${tag#v}-cb.$hash"

src="$build/cloud-spanner-emulator"
if [ -e "$src" ]; then
  echo "$src exists; remove it to rebuild from a fresh clone" >&2
  exit 1
fi
mkdir -p "$build"
git clone --quiet --depth 1 --branch "$tag" "$repo" "$src"
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$commit" ]; then
  echo "tag $tag is $got, sources.json pins $commit" >&2
  exit 1
fi

jq -r '.emulator.patches[]' "$src_json" | while read -r p; do
  git -C "$src" apply --whitespace=nowarn "$dir/$p"
done
# Bazel applies the googlesql patches, concatenated and without their
# headers, to GoogleSQL's archive.
: > "$src/$bazel_patch"
jq -r '.googlesql.patches[]' "$src_json" | while read -r p; do
  sed -n '/^diff --git /,$p' "$dir/$p" >> "$src/$bazel_patch"
done

if [ "$check" = 1 ]; then
  echo "patches apply to $tag ($commit)"
  exit 0
fi

# the upstream Dockerfile up to its Bazel step, then CloudBurrow's tail
df="$build/Dockerfile"
sed '/^# Build GoogleSQL to cache it/,$d' "$src/$dockerfile" > "$df"
cat "$dir/Dockerfile.tail" >> "$df"
args=(build --file "$df" --tag "$image" --load --build-arg "BAZEL_JOBS=${BAZEL_JOBS:-4}")
if [ -n "$platform" ]; then
  args+=(--platform "$platform")
fi
docker buildx "${args[@]}" "$src" >&2
echo "$image"
