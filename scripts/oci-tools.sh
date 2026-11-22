#!/usr/bin/env bash
# oci-tools.sh: fetch the real registry clients
# TestIAMEnforcementArtifactRegistryOCIClients runs (#2111), each pinned in
# dependencies.json's realTools and checked before it is used:
#   - oras, cosign and crane: the linux release binaries, for the docker
#     daemon's architecture, from each project's own GitHub release page,
#     checked against the SHA-256 the release's checksums file publishes
#     (copied into dependencies.json); all three Apache-2.0;
#   - docker:<version>-dind, pulled by its digest (realTools.dockerDind),
#     the image the clients and the relay run in and the docker daemon
#     docker login, push and pull run in.
# A binary already in <dir> whose download matched its pin is not fetched
# again. Nothing here signs in to any registry: the pull is anonymous, and
# the clients are only ever given tokens the local instance minted.
#
# Usage: scripts/oci-tools.sh <dir>
# Prints CB_OCI_TOOLS=<dir>/bin and CB_OCI_CLIENT_IMAGE=docker@sha256:...,
# for $GITHUB_ENV or eval. Requires curl, jq, tar, docker, and sha256sum
# or shasum.
set -euo pipefail

dir=${1:?usage: scripts/oci-tools.sh <dir>}
root=$(cd "$(dirname "$0")/.." && pwd)
deps="$root/dependencies.json"
mkdir -p "$dir/bin" "$dir/dl"
dir=$(cd "$dir" && pwd)

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

arch=$(docker version --format '{{.Server.Arch}}')
case "$arch" in
  amd64) oras_arch=amd64 cosign_arch=amd64 crane_arch=x86_64 ;;
  arm64) oras_arch=arm64 cosign_arch=arm64 crane_arch=arm64 ;;
  *) echo "oci-tools: no pinned binaries for the docker daemon's architecture $arch" >&2; exit 1 ;;
esac

# pin <tool> <asset>: the SHA-256 dependencies.json records for the asset.
pin() {
  local sum
  sum=$(jq -er --arg t "$1" --arg a "$2" '.components.realTools[$t].checksums[$a]' "$deps") ||
    { echo "oci-tools: dependencies.json has no checksum for $1's $2" >&2; exit 1; }
  echo "${sum#sha256:}"
}

# fetch <tool> <url> <asset>: <dir>/dl/<asset>, checked against its pin.
fetch() {
  local tool=$1 url=$2 asset=$3 dest="$dir/dl/$3" sum
  sum=$(pin "$tool" "$asset")
  if [ ! -f "$dest" ] || [ "$(sha256 "$dest")" != "$sum" ]; then
    echo "oci-tools: fetching $url" >&2
    curl --retry 5 --retry-all-errors -fsSLo "$dest.part" "$url"
    mv "$dest.part" "$dest"
  fi
  if [ "$(sha256 "$dest")" != "$sum" ]; then
    echo "oci-tools: $asset does not match its pin (sha256 $sum)" >&2
    rm -f "$dest"
    exit 1
  fi
  echo "oci-tools: $asset matches its pin" >&2
}

version() { jq -er --arg t "$1" '.components.realTools[$t].version' "$deps"; }

v=$(version oras)
asset="oras_${v}_linux_${oras_arch}.tar.gz"
fetch oras "https://github.com/oras-project/oras/releases/download/v${v}/${asset}" "$asset"
tar -xzf "$dir/dl/$asset" -C "$dir/bin" oras

v=$(version cosign)
asset="cosign-linux-${cosign_arch}"
fetch cosign "https://github.com/sigstore/cosign/releases/download/${v}/${asset}" "$asset"
cp "$dir/dl/$asset" "$dir/bin/cosign"

v=$(version crane)
asset="go-containerregistry_Linux_${crane_arch}.tar.gz"
fetch crane "https://github.com/google/go-containerregistry/releases/download/${v}/${asset}" "$asset"
tar -xzf "$dir/dl/$asset" -C "$dir/bin" crane

chmod 0755 "$dir/bin/oras" "$dir/bin/cosign" "$dir/bin/crane"

digest=$(jq -er '.components.realTools.dockerDind.digest' "$deps")
image="docker@$digest"
if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "oci-tools: pulling $(jq -r '.components.realTools.dockerDind.image' "$deps") by digest" >&2
  docker pull -q "$image" >&2
fi

echo "CB_OCI_TOOLS=$dir/bin"
echo "CB_OCI_CLIENT_IMAGE=$image"
