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
# A scheduled release (#1898) commits nothing to main: its notes are
# generated from the landings since the previous tag and kept in its
# annotated tag's message (scripts/scheduled-release.sh). When CHANGELOG.md
# has no section for the tag, the notes are the tag message's body, if it
# starts with the marker line that script writes; any other tag message is
# not release notes, and the tag fails as before. A hand-written CHANGELOG.md
# section always wins. The tag must be in the repository in the current
# directory, as an annotated tag object.
#
# Generated notes get a "What's new" section first, so a release never says
# only which landings it holds. It is the CHANGELOG.md entries added under
# **Unreleased** between the previous vX.Y.Z tag and this one (the entries
# in the tag's Unreleased section that the previous tag's lacks, under their
# ### Added/Changed/Fixed headings), then the issues the landings closed
# that no such entry names (all of them when no entry was added): the issue number of each feat/N-... and
# fix/N-... branch merged since the previous tag (fix/main-ci... branches,
# which fix CI and not the product, are left out) and each "Closes #N" in a
# merge message, grouped by area, the part of the issue's title before its
# first colon. With RELEASE_NOTES_REPO set (owner/repo) the titles are read
# with gh, and an issue labelled workflow-red or titled "CI: ..." is left
# out; without it, or when gh cannot read one, the branch name stands in.
# The landing list itself, "Fix main CI ..." landings included, is folded
# into a collapsed "All landings" block. A hand-written CHANGELOG.md section
# is already curated and is printed as it is. The previous tag and both
# CHANGELOG.md versions are read with git, so the release jobs check out
# with full history.
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

semver='^v[0-9]+\.[0-9]+\.[0-9]+$'

# The newest vX.Y.Z tag in the tag's history, other than the tag and any
# tag of the same commit.
previous_tag() {
	this="$(git rev-parse -q --verify "refs/tags/${tag}^{commit}" 2>/dev/null)" || return 1
	git tag -l 'v*' --sort=-v:refname --merged "${this}" 2>/dev/null | grep -E "${semver}" |
		while read -r t; do
			[ "$(git rev-parse "refs/tags/${t}^{commit}")" = "${this}" ] && continue
			echo "${t}"
			break
		done
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

# The entries added under Unreleased from $1 to the tag, under #### headings.
changelog_news() {
	unreleased_entries "$1" > "${work}/prev"
	unreleased_entries "${tag}" > "${work}/this"
	awk 'BEGIN { RS = "\036"; FS = "\037" }
		NR == FNR { seen[$0] = 1; next }
		$0 == "" || ($0 in seen) { next }
		{
			if (!($1 in n)) { order[++heads] = $1; n[$1] = 0 }
			items[$1, ++n[$1]] = $2
		}
		END {
			for (h = 1; h <= heads; h++) {
				k = order[h]
				if (k != "") printf "\n#### %s\n\n", k; else print ""
				for (i = 1; i <= n[k]; i++) print items[k, i]
			}
		}' "${work}/prev" "${work}/this"
}

# The issues the landings from $1 to the tag closed, by area.
issue_news() {
	git log --merges --format='%s%n%b' "$1..refs/tags/${tag}" > "${work}/merges"
	{
		grep -oE '(feat|fix)/[0-9]+-[A-Za-z0-9._-]*' "${work}/merges" | grep -v '^fix/main-ci' |
			sed -E 's#^((feat|fix)/([0-9]+)-.*)$#\3 \1#'
		grep -oiE '(close[sd]?|fix(e[sd])?|resolve[sd]?) #[0-9]+' "${work}/merges" | sed -E 's/.*#([0-9]+)$/\1 -/'
	} | sort -n -k1,1 -k2,2r | awk '!seen[$1]++' > "${work}/all-issues" || true
	# Issues the CHANGELOG.md entries already name are not listed again.
	grep -oE '#[0-9]+' "${work}/named" 2>/dev/null | tr -d '#' | sort -u > "${work}/named-issues" || true
	awk 'FILENAME == ARGV[1] { named[$1] = 1; next } !($1 in named)' "${work}/named-issues" "${work}/all-issues" > "${work}/issues"

	while read -r n branch; do
		title=""; skip=""
		if [ -n "${RELEASE_NOTES_REPO:-}" ] && command -v gh >/dev/null 2>&1; then
			if info="$(gh issue view "${n}" -R "${RELEASE_NOTES_REPO}" --json title,labels \
				--jq '([.labels[].name] | index("workflow-red") != null | tostring) + "\t" + .title' 2>/dev/null < /dev/null)"; then
				skip="${info%%"${tab}"*}"
				title="${info#*"${tab}"}"
			fi
		fi
		[ "${skip}" = true ] && continue
		case "${title}" in CI:* | "CI "*) continue ;; esac
		area="Other"
		if [ -z "${title}" ]; then
			[ "${branch}" = - ] && title="Issue #${n}" || title="\`${branch}\`"
		else
			prefix="$(printf '%s\n' "${title}" | awk -F': ' 'NF > 1 && length($1) <= 40 { print $1 }')"
			if [ -n "${prefix}" ]; then
				title="${title#"${prefix}": }"
				case "${prefix}" in Console*) area="Console" ;; *) area="${prefix}" ;; esac
			fi
		fi
		printf '%s\t- %s (#%s)\n' "${area}" "${title}" "${n}"
	done < "${work}/issues" | sort -s -t "${tab}" -k1,1 | awk -F "${tab}" '
		{ if (!($1 in n)) order[++k] = $1; items[$1, ++n[$1]] = $2 }
		END {
			# By area, Other last.
			for (pass = 1; pass <= 2; pass++)
				for (a = 1; a <= k; a++) {
					if ((order[a] == "Other") != (pass == 2)) continue
					printf "\n#### %s\n\n", order[a]
					for (i = 1; i <= n[order[a]]; i++) print items[order[a], i]
				}
		}'
}

# The What's new section for generated notes, or nothing.
whats_new() {
	prev="$(previous_tag)" || prev=""
	if [ -z "${prev}" ]; then
		echo "release-notes.sh: no earlier vX.Y.Z tag in ${tag}'s history (a shallow clone?); no What's new section" >&2
		return 0
	fi
	news="$(changelog_news "${prev}")"
	printf '%s\n' "${news}" > "${work}/named"
	issues="$(issue_news "${prev}")"
	[ -n "${news}${issues}" ] || return 0
	echo "### What's new"
	if [ -n "${news}" ]; then
		echo
		echo "The [CHANGELOG.md](${changelog}) entries added under **Unreleased** since ${prev}:"
		printf '%s\n' "${news}"
	fi
	if [ -n "${issues}" ]; then
		echo
		if [ -n "${news}" ]; then
			echo "Also closed by the landings since ${prev}, with no CHANGELOG.md entry:"
		else
			echo "No CHANGELOG.md entry was added since ${prev}; these are the issues its landings closed:"
		fi
		printf '%s\n' "${issues}"
	fi
}

# Generated notes with What's new before their first ### section, and the
# landing list folded into a collapsed block.
with_whats_new() {
	# What's new is extra: a failure to build it never fails the notes.
	whats_new > "${work}/news" || : > "${work}/news"
	printf '%s\n' "$1" | awk -v news="${work}/news" '
		function insert() {
			if (done) return
			done = 1
			while ((getline line < news) > 0) { print line; any = 1 }
			if (any) print ""
		}
		function close_details() {
			if (!folding) return
			if (last != "") print ""
			print "</details>"
			folding = 0
		}
		/^### / || /^<details>/ { insert() }
		/^### / { close_details() }
		$0 == "### Landings" {
			print "<details><summary>All landings</summary>"
			folding = 1; last = "x"
			next
		}
		{ print; last = $0 }
		END {
			if (!done) { print ""; insert() }
			close_details()
		}'
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
	# Leading and trailing blank lines dropped, as for a section.
	section="$(printf '%s\n' "$section" | awk 'NF { seen = 1 } seen { lines[++n] = $0; if (NF) last = n } END { for (i = 1; i <= last; i++) print lines[i] }')"
	work="$(mktemp -d)"
	trap 'rm -rf "${work}"' EXIT
	tab="$(printf '\t')"
	section="$(with_whats_new "$section" | awk 'NF { seen = 1 } seen { lines[++n] = $0; if (NF) last = n } END { for (i = 1; i <= last; i++) print lines[i] }')"
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
