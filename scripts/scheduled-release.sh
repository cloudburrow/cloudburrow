#!/bin/sh
# Decide whether the scheduled release (#1898) releases now, which commit,
# under which version, and write its release notes.
#
#   GH_TOKEN=... GITHUB_REPOSITORY=owner/repo sh scripts/scheduled-release.sh <notes-file>
#
# .github/workflows/scheduled-release.yml runs this in a full clone of main
# with every tag fetched; docs/ci.md#releasing describes the cadence. It
# prints its decision, writes `release`, `tag`, `sha`, `last_tag`, `bump` and
# `reason` to $GITHUB_OUTPUT when that is set, and writes the notes to
# <notes-file> only when it decides to release. It tags and pushes nothing.
#
# The commit: the newest commit on main whose own push run of ci.yml passed
# in full, compat shards included (scripts/require-ci-green.sh --main-push).
# A pull request's ci-green is never enough.
#
# Nothing is released when that commit is already tagged or is not newer
# than the last release, or when the last release (its tag's date) is under
# 24 hours old. From 24 to 48 hours a commit is released only when it is
# main's head, so a release does not go out while newer work is still in
# CI; from 48 hours on the newest fully green commit is released whatever
# main's head is doing. FORCE=true skips the timing rules, never the CI
# rule.
#
# The version: VERSION=vX.Y.Z when given (it must be newer than the last
# tag). Otherwise a minor bump when a feature branch landed since the last
# tag, else a patch bump. A feature branch landed when any merge commit in
# <last tag>..<commit> merged a branch named feat/...: the landing merges
# ("Merge feat/1799-nl (...) into Landing 149"), a pull request from one
# ("Merge pull request #N from owner/feat/..."), or `git merge`'s own
# "Merge branch 'feat/...'". Branch names are the repository's convention
# for a change that adds behaviour (fix/... for one that corrects it), and
# git records them in every merge, so the rule needs no API and no label.
#
# The notes: generated from the merges since the last tag (the landings on
# main's first-parent line, and the feat/ and fix/ branches each one merged),
# behind the marker line scripts/release-notes.sh looks for in an annotated
# tag's message. Nothing is committed to main.
#
# NOW (seconds since the epoch) and MAIN_REF (origin/main) can be set for a
# test. CANDIDATES (30) is how many of main's successful push runs are tried.

set -eu

[ $# -eq 1 ] || { echo "usage: scheduled-release.sh <notes-file>" >&2; exit 2; }
notes="$1"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
main="${MAIN_REF:-origin/main}"
now="${NOW:-$(date +%s)}"
force="${FORCE:-false}"
want_version="${VERSION:-}"
candidates="${CANDIDATES:-30}"
here="$(dirname "$0")"
marker='<!-- cloudburrow-release-notes: generated -->'
feature_merge="^Merge (pull request #[0-9]+ from [^ /]+/|branch '|remote-tracking branch '[^' ]+/)?feat/"

out() {
	echo "$1=$2"
	if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$1=$2" >> "${GITHUB_OUTPUT}"; fi
}

# Whether vX.Y.Z $1 is newer than vX.Y.Z $2.
newer() {
	awk -v a="${1#v}" -v b="${2#v}" 'BEGIN {
		split(a, x, "."); split(b, y, ".")
		for (i = 1; i <= 3; i++) {
			if (x[i] + 0 > y[i] + 0) exit 0
			if (x[i] + 0 < y[i] + 0) exit 1
		}
		exit 1
	}'
}

# No release: say why, and stop.
skip() {
	out release false
	out reason "$1"
	exit 0
}

semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
last_tag="$(git tag -l 'v*' --sort=-v:refname | grep -E "${semver}" | sed -n 1p)"
[ -n "${last_tag}" ] || { echo "scheduled-release.sh: no vX.Y.Z tag to release after" >&2; exit 1; }
last_commit="$(git rev-list -n 1 "${last_tag}")"
last_time="$(git for-each-ref --format='%(creatordate:unix)' "refs/tags/${last_tag}")"
head="$(git rev-parse "${main}")"
out last_tag "${last_tag}"
echo "last release: ${last_tag} at ${last_commit}, $(((now - last_time) / 3600)) hours ago; main is at ${head}"

# The newest commit on main whose push run passed in full. Runs come newest
# first; a commit that is not newer than the last release ends the search.
sha=""
for c in $(gh api "repos/${repo}/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=${candidates}" \
	--jq '.workflow_runs[] | select(.path == ".github/workflows/ci.yml" and .head_branch == "main") | .head_sha' | awk '!seen[$0]++'); do
	if ! git merge-base --is-ancestor "${c}" "${head}" 2>/dev/null; then
		echo "skipping ${c}: not on ${main}"
		continue
	fi
	if git merge-base --is-ancestor "${c}" "${last_commit}"; then
		break
	fi
	if CI_GREEN_TIMEOUT=0 sh "${here}/require-ci-green.sh" --main-push "${c}"; then
		sha="${c}"
		break
	fi
done
[ -n "${sha}" ] || skip "no commit on main newer than ${last_tag} has passed main's full CI run"
out sha "${sha}"

tagged="$(git tag --points-at "${sha}" | grep -E "${semver}" | sed -n 1p || true)"
[ -z "${tagged}" ] || skip "${sha} is already released as ${tagged}"

if [ "${force}" != true ]; then
	age=$((now - last_time))
	if [ "${age}" -lt 86400 ]; then
		skip "${last_tag} is under 24 hours old"
	fi
	if [ "${age}" -lt 172800 ] && [ "${sha}" != "${head}" ]; then
		skip "${last_tag} is under 48 hours old and main's head ${head} has not passed its full CI run yet; ${sha} is released at 48 hours if nothing newer passes"
	fi
fi

# The version.
old="${last_tag#v}"
major="${old%%.*}"
rest="${old#*.}"
minor="${rest%%.*}"
patch="${rest#*.}"
if git log --merges --format=%s "${last_tag}..${sha}" | grep -Eq "${feature_merge}"; then
	bump="minor"
	next="v${major}.$((minor + 1)).0"
else
	bump="patch"
	next="v${major}.${minor}.$((patch + 1))"
fi
if [ -n "${want_version}" ]; then
	printf '%s\n' "${want_version}" | grep -Eq "${semver}" ||
		{ echo "scheduled-release.sh: VERSION '${want_version}' is not vX.Y.Z" >&2; exit 2; }
	if ! newer "${want_version}" "${last_tag}"; then
		echo "scheduled-release.sh: VERSION ${want_version} is not newer than ${last_tag}" >&2
		exit 2
	fi
	bump="override"
	next="${want_version}"
fi
if git rev-parse -q --verify "refs/tags/${next}" >/dev/null; then
	echo "scheduled-release.sh: ${next} already exists" >&2
	exit 1
fi

# The notes.
url="https://github.com/${repo}"
tab="$(printf '\t')"
landings="$(git rev-list --first-parent --merges "${last_tag}..${sha}")"
{
	echo "${marker}"
	echo "Released automatically from [\`$(git rev-parse --short "${sha}")\`](${url}/commit/${sha}) on main, whose full CI run"
	echo "passed, compatibility shards included. Changes since [${last_tag}](${url}/releases/tag/${last_tag})"
	echo "([diff](${url}/compare/${last_tag}...${next})). The curated entries for this work are under"
	echo "**Unreleased** in [CHANGELOG.md](CHANGELOG.md) at this tag; only a row of"
	echo "[docs/compatibility.md](docs/compatibility.md) marked Verified is a support claim."
	# Each branch a landing merged, by kind: "<kind>\t<line>".
	for m in $(git rev-list --merges "${last_tag}..${sha}"); do
		case "
${landings}
" in *"
${m}
"*) continue ;; esac
		git log -1 --format=%s "${m}" | sed -n -E \
			-e "s/^Merge (feat|fix)\/([^ ]+) \((.*)\) into (.*)\$/\1${tab}\3 (\`\1\/\2\`, \4)/p" \
			-e "s/^Merge branch '(feat|fix)\/([^']+)'.*\$/\1${tab}\`\1\/\2\`/p"
	done > "${notes}.branches"
	for kind in feat:Added fix:Fixed; do
		lines="$(awk -F "${tab}" -v k="${kind%%:*}" '$1 == k { print "- " $2 }' "${notes}.branches")"
		if [ -n "${lines}" ]; then
			echo
			echo "### ${kind#*:}"
			echo
			printf '%s\n' "${lines}"
		fi
	done
	rm -f "${notes}.branches"
	if [ -n "${landings}" ]; then
		echo
		echo "### Landings"
		echo
		for m in ${landings}; do
			subject="$(git log -1 --format=%s "${m}")"
			title="$(git log -1 --format=%b "${m}" | sed -n 1p)"
			pr="$(printf '%s\n' "${subject}" | sed -n 's/^Merge pull request #\([0-9][0-9]*\) from .*/#\1/p')"
			echo "- ${title:-${subject}}${pr:+ (${pr})}"
		done
	fi
	direct="$(git log --first-parent --no-merges --format='- %s (%h)' "${last_tag}..${sha}")"
	if [ -n "${direct}" ]; then
		echo
		echo "### Other commits"
		echo
		printf '%s\n' "${direct}"
	fi
} > "${notes}"

out release true
out tag "${next}"
out bump "${bump}"
out reason "${sha} passed main's full CI run; ${bump} bump from ${last_tag}"
