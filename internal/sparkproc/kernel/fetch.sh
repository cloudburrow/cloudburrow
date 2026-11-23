#!/bin/bash
# fetch.sh LOCK DIR: every jar of LOCK (lines "SHA256 URL"), into DIR as
# NNN-NAME in the lock's order, each checked against its SHA-256; any
# mismatch fails the image build (internal/sparkproc, #2162).
set -euo pipefail
lock=$1 dir=$2 n=0
mkdir -p "$dir"
while read -r sum url; do
  case "$sum" in ''|'#'*) continue ;; esac
  n=$((n + 1))
  f="$dir/$(printf '%03d' "$n")-${url##*/}"
  curl -fsSL --retry 3 -o "$f" "$url"
  echo "$sum  $f" | sha256sum -c --quiet -
done < "$lock"
