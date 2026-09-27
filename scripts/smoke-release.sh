#!/bin/sh
# Install a published release the way a user would, and check what landed.
#
#   sh scripts/smoke-release.sh v0.1.0
#
# The installer is fetched from the tag's own raw URL, not from this
# checkout, and runs against the real release: the archive's checksum and
# its build attestation must both be verified, so `gh` must be installed and
# authenticated (GH_TOKEN in CI). Then the installed binary must report
# exactly the tag, and `cloudburrow doctor` must run to a report: a host
# without Docker gets a blocking report and exit status 1, which is a
# working doctor; any other failure is not.
#
# The release workflow runs this after publishing (#603). It needs nothing
# from this checkout but itself, so it can be run by hand against any tag.

set -eu

[ $# -eq 1 ] || { echo "usage: smoke-release.sh <tag>" >&2; exit 2; }
tag="$1"
repo="${SMOKE_REPOSITORY:-cloudburrow/cloudburrow}"

say() { printf 'smoke-release: %s\n' "$*" >&2; }
die() { say "FAIL: $*"; exit 1; }

command -v gh >/dev/null 2>&1 || die "gh is required: the smoke test must verify the build attestation"
[ -z "${CLOUDBURROW_RELEASE_BASE:-}" ] || die "CLOUDBURROW_RELEASE_BASE is set; the smoke test installs the published release"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

url="https://raw.githubusercontent.com/${repo}/${tag}/scripts/install.sh"
say "installing ${tag} with ${url}"
curl -fsSL -o "${tmp}/install.sh" "$url" || die "could not download ${url}"
sh "${tmp}/install.sh" --version "$tag" --prefix "${tmp}/prefix" 2>"${tmp}/install.log" ||
	{ cat "${tmp}/install.log" >&2; die "the installer failed"; }
cat "${tmp}/install.log" >&2
grep -q 'checksum verified' "${tmp}/install.log" || die "the installer did not verify the checksum"
grep -q 'build attestation verified' "${tmp}/install.log" || die "the installer did not verify the build attestation"

bin="${tmp}/prefix/bin/cloudburrow"
"$bin" version >&2 || die "cloudburrow version failed"
got="$("$bin" version --short)" || die "cloudburrow version --short failed"
[ "$got" = "$tag" ] || die "the installed binary reports ${got}, not ${tag}"
say "cloudburrow version --short: ${got}"

status=0
"$bin" doctor >"${tmp}/doctor.log" 2>&1 || status=$?
cat "${tmp}/doctor.log" >&2
grep -q 'cloudburrow doctor: checking prerequisites' "${tmp}/doctor.log" || die "doctor did not start its report"
case "$status" in
0) say "doctor: no blocking problems" ;;
1)
	grep -q 'blocking problems found' "${tmp}/doctor.log" || die "doctor exited 1 without a blocking report"
	say "doctor ran and reported blocking problems on this host (report above)"
	;;
*) die "doctor exited ${status}" ;;
esac

say "ok: ${tag} installs, verifies and runs"
