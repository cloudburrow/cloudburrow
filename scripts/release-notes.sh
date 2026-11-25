#!/bin/sh
# Print a release's notes: its section of CHANGELOG.md, or the notes a
# scheduled release generated from CHANGELOG.md.
#
#   sh scripts/release-notes.sh v0.1.0 [CHANGELOG.md]
#   sh scripts/release-notes.sh --unreleased <from-ref> <to-ref> [CHANGELOG.md]
#
# The section is the one headed `## [0.1.0]` (Keep a Changelog form), up to
# the next `## ` heading or the link references at the end of the file. It
# fails, printing nothing, when the tag has no section or its section is
# empty, so a release cannot be published with notes that say nothing about
# what changed.
#
# A scheduled release (#1898) commits nothing to main: its notes are kept in
# its annotated tag's message (scripts/scheduled-release.sh), and they are
# the CHANGELOG.md entries written under **Unreleased** since the previous
# tag, which that script reads with --unreleased. When CHANGELOG.md has no
# section for the tag, the notes are the tag message's body, if it starts
# with the marker line that script writes and holds at least one entry
# under a ### heading of its own; any other tag message is not release
# notes, and the tag fails as before. A hand-written CHANGELOG.md section
# always wins. The tag must be in the repository in the current directory,
# as an annotated tag object.
#
# Nothing is made up from branch names, merge subjects or issue titles: they
# name work in progress and problems, not what a release does. With no
# entries, --unreleased fails with "CHANGELOG Unreleased is empty", and the
# release with it, so the operator writes them (CONTRIBUTING.md#landings).
#
# --unreleased prints the entries of the Unreleased section of CHANGELOG.md
# at <to-ref> that its Unreleased section at <from-ref> lacks, under their
# ### headings (an entry under no heading goes under "### Changes"). Both
# versions are read with git, so the jobs that run it check out with full
# history.
#
# With RELEASE_NOTES_LINK_BASE set (such as
# https://github.com/cloudburrow/cloudburrow/blob/v0.1.0), relative Markdown
# links are made absolute against it: a release page resolves a relative link
# against itself, where docs/status.md does not exist.

set -eu

usage() {
	echo "usage: release-notes.sh <tag> [CHANGELOG.md]" >&2
	echo "       release-notes.sh --unreleased <from-ref> <to-ref> [CHANGELOG.md]" >&2
	exit 2
}

# The entries of a ref's Unreleased section, each as "heading<US>entry<RS>":
# an entry is a "- " line and its continuation lines.
unreleased_entries() {
	git show "$1:./${changelog}" 2>/dev/null | awk '
		function flush() { if (cur != "") printf "%s\037%s\036", head, cur; cur = "" }
		/^## \[Unreleased\]/ { inside = 1; next }
		!inside { next }
		/^## / || /^\[[^]]+\]: / { flush(); exit }
		/^### / { flush(); head = substr($0, 5); next }
		/^- / { flush(); cur = $0; next }
		/^[[:space:]]*$/ { flush(); next }
		cur != "" { cur = cur "\n" $0 }
		END { flush() }
	'
}

if [ "${1:-}" = --unreleased ]; then
	[ $# -eq 3 ] || [ $# -eq 4 ] || usage
	from="$2"
	to="$3"
	changelog="${4:-CHANGELOG.md}"
	git rev-parse -q --verify "${to}^{commit}" >/dev/null ||
		{ echo "release-notes.sh: ${to} is not a commit" >&2; exit 1; }
	work="$(mktemp -d)"
	trap 'rm -rf "${work}"' EXIT
	unreleased_entries "${from}" > "${work}/prev"
	unreleased_entries "${to}" > "${work}/this"
	awk 'BEGIN { RS = "\036"; FS = "\037" }
		FILENAME == ARGV[1] { seen[$0] = 1; next }
		$0 == "" || ($0 in seen) { next }
		{
			h = $1 == "" ? "Changes" : $1
			if (!(h in n)) { order[++heads] = h; n[h] = 0 }
			items[h, ++n[h]] = $2
		}
		END {
			for (i = 1; i <= heads; i++) {
				k = order[i]
				if (i > 1) print ""
				printf "### %s\n\n", k
				for (j = 1; j <= n[k]; j++) print items[k, j]
			}
		}' "${work}/prev" "${work}/this" > "${work}/news"
	if [ ! -s "${work}/news" ]; then
		echo "release-notes.sh: CHANGELOG Unreleased is empty: no entry was added under ## [Unreleased] in ${changelog} from ${from} to ${to}; write the release's entries there and land them first" >&2
		exit 1
	fi
	cat "${work}/news"
	exit 0
fi

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	usage
fi
tag="$1"
changelog="${2:-CHANGELOG.md}"

case "$tag" in
v[0-9]*) ;;
*) echo "release-notes.sh: '${tag}' is not a release tag (vX.Y.Z)" >&2; exit 2 ;;
esac
[ -r "$changelog" ] || { echo "release-notes.sh: cannot read ${changelog}" >&2; exit 1; }

# The marker scripts/scheduled-release.sh puts on the first line of a
# generated tag message's body. An HTML comment, so it renders as nothing.
marker='<!-- cloudburrow-release-notes: generated -->'

# The generated notes in the tag's annotated message, without the marker.
tag_notes() {
	[ "$(git cat-file -t "refs/tags/${tag}" 2>/dev/null)" = tag ] || return 1
	body="$(git for-each-ref --format='%(contents:body)' "refs/tags/${tag}")"
	[ "$(printf '%s\n' "$body" | sed -n 1p)" = "$marker" ] || return 1
	printf '%s\n' "$body" | sed 1d
}

# Whether generated notes hold a CHANGELOG.md entry: a "- " line under a ###
# heading, outside any <details> block (where the landing list is).
has_entries() {
	printf '%s\n' "$1" | awk '
		/^<details>/ { folded = 1 } /^<\/details>/ { folded = 0; next }
		folded { next }
		/^### / { under = 1; next }
		under && /^- / { found = 1; exit }
		END { exit !found }'
}

# Matched as a string, not a pattern, so the dots in a version match only
# dots, and 0.1.0 never matches the heading of 0.1.0-rc.1 or 0.1.01.
if section="$(awk -v want="## [${tag#v}]" '
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
' "$changelog")"; then
	:
elif section="$(tag_notes)"; then
	if ! has_entries "${section}"; then
		echo "release-notes.sh: ${tag}'s generated notes hold no CHANGELOG.md entry (CHANGELOG Unreleased is empty); add a '## [${tag#v}]' section to ${changelog} rather than publish notes that say nothing" >&2
		exit 1
	fi
	# Leading and trailing blank lines dropped, as for a section.
	section="$(printf '%s\n' "$section" | awk 'NF { seen = 1 } seen { lines[++n] = $0; if (NF) last = n } END { for (i = 1; i <= last; i++) print lines[i] }')"
else
	echo "release-notes.sh: ${changelog} has no '## [${tag#v}]' section for ${tag}, and ${tag} is not a scheduled release's annotated tag with generated notes; add a section before tagging" >&2
	exit 1
fi

[ -n "$section" ] || { echo "release-notes.sh: the ${tag} section of ${changelog} is empty" >&2; exit 1; }
if [ -n "${RELEASE_NOTES_LINK_BASE:-}" ]; then
	# A link target with no colon and not starting with # is a path in the
	# repository; URLs and in-page anchors are left alone.
	printf '%s\n' "$section" | sed "s#](\([^):\#][^):]*\))#](${RELEASE_NOTES_LINK_BASE%/}/\1)#g"
else
	printf '%s\n' "$section"
fi
