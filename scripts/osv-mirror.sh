#!/bin/sh
# The OSV mirror and pin bump (#1917), for release.yml and osv-pins.yml.
#
#   osv-mirror.sh stage TAG DIR    stage the mirror of TAG's pins in DIR
#                                  (tools/osvmirror stage), trying upstream
#                                  and then the newest earlier releases, and
#                                  check DIR against its SHA256SUMS
#   osv-mirror.sh upload TAG DIR   upload DIR's files to release TAG
#   osv-mirror.sh propose LOG      after `tools/osvmirror bump` changed the
#                                  pins: commit them to a new branch
#                                  auto/osv-pins-<date>, push it, and open a
#                                  pull request, or, where GitHub Actions may
#                                  not open one, an issue naming the branch
#
# Everything uses gh with the workflow's own GITHUB_TOKEN (GH_TOKEN) and
# GITHUB_REPOSITORY; nothing here needs another secret. OSV_MIRROR_EARLIER
# is how many earlier releases stage falls back to (default 5).
set -eu

repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"

sha256_check() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c "$1"
  else
    shasum -a 256 -c "$1"
  fi
}

# earlier_bases TAG: the download bases of the newest published releases
# other than TAG, comma-separated.
earlier_bases() {
  gh release list --repo "$repo" --exclude-drafts --limit 20 --json tagName --jq '.[].tagName' |
    grep -vxF "$1" | head -n "${OSV_MIRROR_EARLIER:-5}" |
    sed "s|^|https://github.com/${repo}/releases/download/|" | paste -sd, -
}

case "${1:-}" in
stage)
  tag="${2:?usage: osv-mirror.sh stage TAG DIR}"
  dir="${3:?usage: osv-mirror.sh stage TAG DIR}"
  bases="$(earlier_bases "$tag")"
  echo "falling back to: ${bases:-(no earlier release)}"
  go run ./tools/osvmirror stage -out "$dir" -mirror "$bases"
  (cd "$dir" && sha256_check osv-mirror-SHA256SUMS)
  ;;
upload)
  tag="${2:?usage: osv-mirror.sh upload TAG DIR}"
  dir="${3:?usage: osv-mirror.sh upload TAG DIR}"
  # --clobber so a re-run completes a partial upload: the names are
  # content-addressed, so a name never changes its bytes.
  gh release upload "$tag" --repo "$repo" --clobber "$dir"/*
  echo "uploaded $(find "$dir" -type f | wc -l | tr -d ' ') mirror files to ${tag}"
  ;;
propose)
  log="${2:?usage: osv-mirror.sh propose BUMP_LOG}"
  if git diff --quiet -- internal/osvscan/pins.go dependencies.json; then
    echo "the pins did not move: nothing to propose"
    exit 0
  fi
  date="$(sed -n 's/^const SnapshotDate = "\([0-9-]*\)"$/\1/p' internal/osvscan/pins.go)"
  [ -n "$date" ] || { echo "no SnapshotDate in pins.go" >&2; exit 1; }
  branch="auto/osv-pins-${date}"
  if git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
    echo "${branch} is already pushed: nothing to do"
    exit 0
  fi
  base="$(git rev-parse --abbrev-ref HEAD)"
  title="OSV database pins: move to ${date}"
  msg="$(mktemp)"
  body="$(mktemp)"
  trap 'rm -f "$msg" "$body"' EXIT
  {
    echo "osvscan: move the OSV database pins to ${date}"
    echo
    echo "OSV keeps a pinned generation for about a week (#1917); these are the"
    echo "generations it served on ${date}, each downloaded and hashed by"
    echo "tools/osvmirror bump."
    echo
    grep -E '^[A-Za-z]+: generation ' "$log" || true
  } >"$msg"
  git config user.name "github-actions[bot]"
  git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
  git switch -c "$branch"
  git add internal/osvscan/pins.go dependencies.json
  git commit -q -F "$msg"
  git push origin "$branch"
  {
    sed 1,2d "$msg"
    echo
    echo "Opened by .github/workflows/osv-pins.yml. A push with the workflow's token starts no CI run:"
    echo "land the branch as any other, and its CI run checks the new pins."
  } >"$body"
  if gh pr create --repo "$repo" --base "$base" --head "$branch" --title "$title" --body-file "$body"; then
    exit 0
  fi
  # The repository does not let GitHub Actions create pull requests
  # (Settings > Actions > General). The branch is pushed; an issue says so.
  # The setting is not changed from here.
  echo "::warning::GitHub Actions may not open pull requests in ${repo}; filing an issue for ${branch} instead"
  {
    echo
    echo "Branch: [${branch}](https://github.com/${repo}/compare/${base}...${branch})"
    echo
    echo "GitHub Actions may not open pull requests in this repository, so this issue stands in for one."
  } >>"$body"
  existing="$(gh issue list --repo "$repo" --state open --search "in:title \"OSV database pins: move to\"" --json number --jq '.[0].number // empty')"
  if [ -n "$existing" ]; then
    gh issue comment "$existing" --repo "$repo" --body-file "$body"
    gh issue edit "$existing" --repo "$repo" --title "$title"
  else
    gh issue create --repo "$repo" --title "$title" --body-file "$body"
  fi
  ;;
*)
  echo "usage: osv-mirror.sh stage TAG DIR | upload TAG DIR | propose BUMP_LOG" >&2
  exit 2
  ;;
esac
