#!/bin/sh
# Join hard-wrapped Markdown into one line per paragraph and per list item.
#
#   sh scripts/unwrap-markdown.sh < wrapped.md > unwrapped.md
#
# CHANGELOG.md is wrapped at about 100 columns, and a GitHub release body
# shows every newline as a line break, so release notes built from it break
# mid-sentence. scripts/scheduled-release.sh and release.yml pass the notes
# through this.
#
# A list item ("- ", "* ", "+ " or "1. ", indented or not) starts a new line
# and its continuation lines are joined to it with one space; so are a
# paragraph's lines. Blank lines, headings, code fences and what is inside
# them, and lines that start with "<" (HTML: "<details>", "<!-- ... -->"),
# "|" (tables) or ">" (block quotes) are left as they are. Running it again
# changes nothing.

set -eu

awk '
	function flush() { if (cur != "") print cur; cur = "" }
	function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
	/^[[:space:]]*(```|~~~)/ { flush(); print; fence = !fence; next }
	fence { print; next }
	/^[[:space:]]*$/ { flush(); print; next }
	/^#+([[:space:]]|$)/ || /^(<|\||>)/ { flush(); print; next }
	/^[[:space:]]*([-*+]|[0-9]+[.)])[[:space:]]/ { flush(); cur = $0; sub(/[[:space:]]+$/, "", cur); next }
	cur != "" { cur = cur " " trim($0); next }
	{ cur = $0; sub(/[[:space:]]+$/, "", cur) }
	END { flush() }
'
