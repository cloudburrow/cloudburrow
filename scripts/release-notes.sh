#!/bin/sh
# Print a release's section of CHANGELOG.md, for its release notes.
#
#   sh scripts/release-notes.sh v0.1.0 [CHANGELOG.md]
#
# The section is the one headed `## [0.1.0]` (Keep a Changelog form), up to
# the next `## ` heading or the link references at the end of the file. It
# fails, printing nothing, when the tag has no section or its section is
# empty, so a release cannot be published with notes that say nothing about
# what changed.
#
# With RELEASE_NOTES_LINK_BASE set (such as
# https://github.com/cloudburrow/cloudburrow/blob/v0.1.0), relative Markdown
# links are made absolute against it: a release page resolves a relative link
# against itself, where docs/status.md does not exist.

set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: release-notes.sh <tag> [CHANGELOG.md]" >&2
	exit 2
fi
tag="$1"
changelog="${2:-CHANGELOG.md}"

case "$tag" in
v[0-9]*) ;;
*) echo "release-notes.sh: '${tag}' is not a release tag (vX.Y.Z)" >&2; exit 2 ;;
esac
[ -r "$changelog" ] || { echo "release-notes.sh: cannot read ${changelog}" >&2; exit 1; }

# Matched as a string, not a pattern, so the dots in a version match only
# dots, and 0.1.0 never matches the heading of 0.1.0-rc.1 or 0.1.01.
section="$(awk -v want="## [${tag#v}]" '
	function heading(line) { return substr(line, 1, 3) == "## " }
	!found {
		if (index($0, want) == 1) {
			rest = substr($0, length(want) + 1)
			if (rest == "" || substr(rest, 1, 1) == " ") found = 1
		}
		next
	}
	heading($0) || /^\[[^]]+\]: / { exit }
	{ lines[++n] = $0 }
	END {
		if (!found) exit 3
		first = 1; while (first <= n && lines[first] ~ /^[[:space:]]*$/) first++
		last = n; while (last >= first && lines[last] ~ /^[[:space:]]*$/) last--
		for (i = first; i <= last; i++) print lines[i]
	}
' "$changelog")" || {
	echo "release-notes.sh: ${changelog} has no '## [${tag#v}]' section for ${tag}; add one before tagging" >&2
	exit 1
}

[ -n "$section" ] || { echo "release-notes.sh: the ${tag} section of ${changelog} is empty" >&2; exit 1; }
if [ -n "${RELEASE_NOTES_LINK_BASE:-}" ]; then
	# A link target with no colon and not starting with # is a path in the
	# repository; URLs and in-page anchors are left alone.
	printf '%s\n' "$section" | sed "s#](\([^):\#][^):]*\))#](${RELEASE_NOTES_LINK_BASE%/}/\1)#g"
else
	printf '%s\n' "$section"
fi
