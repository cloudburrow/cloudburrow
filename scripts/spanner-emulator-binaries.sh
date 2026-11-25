#!/usr/bin/env bash
# Build CloudBurrow's patched Cloud Spanner emulator for the CLI to embed
# (#2029), as `make bigquery-binaries` builds the BigQuery emulator.
#
# Google's emulator (Apache-2.0) at the tag third_party/spanner-emulator/
# sources.json pins, its commit checked, with CloudBurrow's patches applied in
# sources.json order, built with the emulator's own Dockerfile up to its
# Bazel step and third_party/spanner-emulator/Dockerfile.tail after it, whose
# Bazel output base is a BuildKit cache mount: GoogleSQL alone takes about an
# hour on 8 CPUs, and an interrupted build keeps what it compiled.
#
# It writes internal/spannerimage/bin/spanner-emulator-<version>-linux-<arch>.tar.gz
# (emulator_main, gateway_main and the licence bundle), where <version> is internal/spannerimage.Version: the upstream
# release and a hash of the patches, so a CLI ignores a build of other patches.
#
# Usage: scripts/spanner-emulator-binaries.sh -arch amd64|arm64 [-out dir] [-test] [-check]
#   -out    where the archive goes (default internal/spannerimage/bin)
#   -test   also run the patches' C++ regression tests (the patch-tests stage)
#   -check  only clone, verify and apply the patches; build nothing.
# Env: SPANNER_EMULATOR_BUILD_DIR (default third_party/spanner-emulator/build),
#      BAZEL_JOBS (default 4: each GoogleSQL compiler can take gigabytes).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
dir="$root/third_party/spanner-emulator"
src_json="$dir/sources.json"
build="${SPANNER_EMULATOR_BUILD_DIR:-$dir/build}"
out="$root/internal/spannerimage/bin"
arch=""
check=0
test=0
while [ $# -gt 0 ]; do
  case "$1" in
    -arch) arch="$2"; shift 2 ;;
    -out) out="$2"; shift 2 ;;
    -test) test=1; shift ;;
    -check) check=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$arch" in
  amd64|arm64) ;;
  *) [ "$check" = 1 ] || { echo "-arch amd64 or -arch arm64 is required" >&2; exit 2; } ;;
esac

repo=$(jq -r .emulator.repository "$src_json")
tag=$(jq -r .emulator.tag "$src_json")
commit=$(jq -r .emulator.commit "$src_json")
dockerfile=$(jq -r .emulator.dockerfile "$src_json")
bazel_patch=$(jq -r .googlesql.bazelPatch "$src_json")

# The version names the upstream tag and a hash of every patch, in order.
hash=$( { jq -r '.emulator.patches[], .googlesql.patches[]' "$src_json" | while read -r p; do
  cat "$dir/$p"; done; } | shasum -a 256 | cut -c1-12)
version="${tag#v}-cb.$hash"

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

# the upstream Dockerfile up to its Bazel step, then CloudBurrow's tail
df="$build/Dockerfile"
sed '/^# Build GoogleSQL to cache it/,$d' "$src/$dockerfile" > "$df"
grep -q -i -E '^FROM .* AS build$' "$df" || { echo "$dockerfile has no stage named build" >&2; exit 1; }
cat "$dir/Dockerfile.tail" >> "$df"

if [ "$check" = 1 ]; then
  echo "patches apply to $tag ($commit); version $version"
  exit 0
fi

common=(--file "$df" --platform "linux/$arch" --progress plain --build-arg "BAZEL_JOBS=${BAZEL_JOBS:-4}")
bin="$build/out-$arch"
docker buildx build "${common[@]}" --target binaries --output "type=local,dest=$bin" "$src" >&2
for f in emulator_main gateway_main licenses.txt.gz; do
  [ -s "$bin/$f" ] || { echo "the build has no $f" >&2; exit 1; }
done
if [ "$test" = 1 ]; then
  docker buildx build "${common[@]}" --target patch-tests "$src" >&2
fi
mkdir -p "$out"
archive="$out/spanner-emulator-$version-linux-$arch.tar.gz"
tar -czf "$archive" -C "$bin" emulator_main gateway_main licenses.txt.gz
echo "$archive"
