#!/bin/sh
# Refuse to release a commit that CI has not passed (#596).
#
#   GH_TOKEN=... GITHUB_REPOSITORY=owner/repo sh scripts/require-ci-green.sh <sha>
#
# The release workflow runs this first, before anything is built or pushed.
# It passes when the commit has a completed, successful `ci-green` check run:
# the one check the merge queue requires, which fails when any CI job failed
# or was cancelled.
#
# Which run proves it. The merge queue fast-forwards main to the exact commit
# it tested, so a merged commit already has a successful ci-green from its
# merge_group run before the tag can exist. Its later push run on main is
# often cancelled by the next merge (CI's concurrency group), so one
# successful ci-green is enough: a cancelled or failed run beside it does not
# undo a pass on the same tree.
#
# Waiting. When no ci-green has passed yet but one is still running, or a CI
# workflow run for the commit has not reached ci-green, this polls every
# CI_GREEN_INTERVAL seconds (60) for up to CI_GREEN_TIMEOUT seconds (2700,
# 45 minutes; a full CI run takes about 25). A commit with no CI run at all,
# or whose every ci-green finished without passing, fails at once: waiting
# cannot change either.

set -eu

[ $# -eq 1 ] || { echo "usage: require-ci-green.sh <sha>" >&2; exit 2; }
sha="$1"
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
timeout="${CI_GREEN_TIMEOUT:-2700}"
interval="${CI_GREEN_INTERVAL:-60}"

# One line per ci-green check run created by GitHub Actions for the commit:
# "<status> <conclusion> <url>". Another app cannot pass for it by naming a
# check ci-green.
check_runs() {
	gh api --paginate \
		"repos/${repo}/commits/${sha}/check-runs?check_name=ci-green&filter=all&per_page=100" \
		--jq '.check_runs[] | select(.app.slug == "github-actions") | "\(.status) \(.conclusion // "none") \(.html_url)"'
}

# The status of each CI workflow run for the commit. ci-green waits on every
# other job, so while they run it may have no check run yet.
ci_runs() {
	gh api --paginate \
		"repos/${repo}/actions/runs?head_sha=${sha}&per_page=100" \
		--jq '.workflow_runs[] | select(.path == ".github/workflows/ci.yml") | .status'
}

waited=0
while :; do
	if runs="$(check_runs)"; then
		passed="$(printf '%s\n' "${runs}" | awk '$1 == "completed" && $2 == "success" { print $3; exit }')"
		if [ -n "${passed}" ]; then
			echo "ci-green passed for ${sha}: ${passed}"
			exit 0
		fi
		pending="$(printf '%s\n' "${runs}" | awk 'NF && $1 != "completed"' | wc -l | tr -d ' ')"
		if [ -z "${runs}" ]; then
			if ! ci="$(ci_runs)"; then
				echo "::warning::could not list the CI workflow runs for ${sha}; retrying"
				pending=1
			else
				pending="$(printf '%s\n' "${ci}" | awk 'NF && $1 != "completed"' | wc -l | tr -d ' ')"
				if [ "${pending}" -eq 0 ]; then
					echo "::error::Refusing to release ${sha}: CI has no ci-green check run for this commit, and no CI run is in progress for it. Release only a commit that has passed CI: one merged through the merge queue, or a branch whose CI has run."
					exit 1
				fi
			fi
		elif [ "${pending}" -eq 0 ]; then
			echo "::error::Refusing to release ${sha}: no ci-green check run for this commit succeeded, and none is still running."
			printf '%s\n' "${runs}" | awk '{ printf "  ci-green %s: %s\n", $2, $3 }'
			exit 1
		fi
	else
		echo "::warning::could not read the check runs for ${sha}; retrying"
	fi

	if [ "${waited}" -ge "${timeout}" ]; then
		echo "::error::Refusing to release ${sha}: CI for this commit had not produced a successful ci-green after ${waited}s. Re-run the release once it has."
		exit 1
	fi
	echo "CI for ${sha} is still running; checking again in ${interval}s (${waited}s of ${timeout}s waited)"
	sleep "${interval}"
	waited=$((waited + interval))
done
