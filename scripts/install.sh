#!/bin/sh
# Install a CloudBurrow release binary, verifying it before anything is
# installed.
#
#   curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh
#   sh install.sh --version v0.1.0 --prefix /usr/local
#
# The archive's SHA-256 must match the release's checksums.txt, or nothing is
# installed. When the GitHub CLI is available, the archive's build attestation
# is verified too, which proves it was built by this repository's release
# workflow and not merely that it matches a checksum file served beside it.
#
# Options:
#   --version TAG   release to install (default: the latest)
#   --prefix DIR    install to DIR/bin (default: ~/.local, so ~/.local/bin)
#   --no-attest     skip attestation verification even when gh is present
#
# CLOUDBURROW_RELEASE_BASE overrides where releases are downloaded from. It
# exists for the installer's own tests, which serve a deliberately corrupted
# archive from a local server; it is not needed to install.

set -eu

repo="cloudburrow/cloudburrow"
version=""
prefix="${HOME}/.local"
attest=1

say() { printf 'cloudburrow-install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
	--version=*) version="${1#*=}"; shift ;;
	--prefix) [ $# -ge 2 ] || die "--prefix needs a value"; prefix="$2"; shift 2 ;;
	--prefix=*) prefix="${1#*=}"; shift ;;
	--no-attest) attest=0; shift ;;
	-h | --help) sed -n '2,21p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported operating system: $(uname -s); releases are built for macOS and Linux" ;;
esac
case "$(uname -m)" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) die "unsupported architecture: $(uname -m); releases are built for arm64 and amd64" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "sha256sum or shasum is required to verify the download"
fi

base="${CLOUDBURROW_RELEASE_BASE:-https://github.com/${repo}/releases}"

if [ -z "$version" ]; then
	# The latest release's page redirects to its tag; the tag is read from
	# where it lands rather than from the API, which rate-limits anonymous
	# callers.
	latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${base}/latest")" ||
		die "could not find the latest release at ${base}/latest"
	version="${latest##*/}"
	case "$version" in
	v*) ;;
	*) die "could not read a release tag from ${latest}" ;;
	esac
fi

name="cloudburrow_${version}_${os}_${arch}"
archive="${name}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading ${archive} (${version})"
curl -fsSL -o "${tmp}/${archive}" "${base}/download/${version}/${archive}" ||
	die "download failed: ${base}/download/${version}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${base}/download/${version}/checksums.txt" ||
	die "download failed: ${base}/download/${version}/checksums.txt"

want="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "${tmp}/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for ${archive}; refusing to install"
got="$(sha256 "${tmp}/${archive}")"
if [ "$got" != "$want" ]; then
	die "checksum mismatch for ${archive}: expected ${want}, got ${got}; refusing to install"
fi
say "checksum verified"

if [ "$attest" = 1 ] && command -v gh >/dev/null 2>&1 && [ -z "${CLOUDBURROW_RELEASE_BASE:-}" ]; then
	gh attestation verify "${tmp}/${archive}" --repo "$repo" >/dev/null ||
		die "attestation verification failed for ${archive}; refusing to install"
	say "build attestation verified"
elif [ "$attest" = 1 ]; then
	say "gh is not installed, so the build attestation was not checked (the checksum was)"
fi

tar -xzf "${tmp}/${archive}" -C "$tmp"
[ -f "${tmp}/${name}/cloudburrow" ] || die "the archive does not contain ${name}/cloudburrow"

mkdir -p "${prefix}/bin"
# Installed under a temporary name and renamed, so an interrupted install
# never leaves a half-written binary where the old one was.
cp "${tmp}/${name}/cloudburrow" "${prefix}/bin/.cloudburrow.new"
chmod 0755 "${prefix}/bin/.cloudburrow.new"
mv "${prefix}/bin/.cloudburrow.new" "${prefix}/bin/cloudburrow"
say "installed ${prefix}/bin/cloudburrow"

case ":${PATH}:" in
*":${prefix}/bin:"*) ;;
*) say "${prefix}/bin is not on your PATH; add it, or run ${prefix}/bin/cloudburrow" ;;
esac
