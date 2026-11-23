#!/bin/sh
# Refuse to release a commit that CI has not passed (#596, #1898).
#
#   GH_TOKEN=... GITHUB_REPOSITORY=owner/repo sh scripts/require-ci-green.sh [--main-push] <sha>
#
# The release workflow runs this first, before anything is built or pushed.
#
# --main-push, which every real release uses (#1898): the commit needs a
# completed `ci.yml` run of its own push to main whose every job passed or
# was skipped by design, with the compatibility shards, their coverage check,
# the integration suite and ci-green among those that passed. A pull
# request's ci-green is not enough: pull-request CI runs the fast jobs only
# (ci.yml, "detect changes"), and a release once shipped from a green PR
# whose main CI then failed. A push run cancelled by the next push to main
# (CI's concurrency group) proves nothing, so such a commit is refused and
# the scheduled release picks an older one that has a full run. It waits
# while a push run for the commit is in progress, for up to CI_GREEN_TIMEOUT
# seconds (7200 in this mode: main's full run takes 80-100 minutes); with
# CI_GREEN_TIMEOUT=0 it answers at once, which is how
# scripts/scheduled-release.sh asks about each candidate commit.
#
# Without --main-push (a manual dry run of release.yml on a branch), it
# passes when the commit has a completed, successful `ci-green` check run:
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
#
# Listing. Every API listing is tried up to CI_GREEN_TRIES times (3),
# CI_GREEN_PAUSE seconds (5) apart, even with CI_GREEN_TIMEOUT=0, and a failed
# try's error from gh is kept in its warning. The jobs of a run with several
# attempts are slow to list: for main's run 45202563361 (attempt 3) the
# endpoint took 5-11s and often answered HTTP 502 at GitHub's 10s limit, so
# the release refused a commit that had passed. When the jobs cannot be
# listed, the latest check runs of the run's check suite stand in for them:
# one per job, named as the job, from the latest attempt, listed in under 1s.

set -eu

mode=any
if [ "${1:-}" = "--main-push" ]; then
	mode=main
	shift
fi
[ $# -eq 1 ] || { echo "usage: require-ci-green.sh [--main-push] <sha>" >&2; exit 2; }
sha="$1"
case "${sha}" in
*[!0-9a-f]* | "") echo "require-ci-green.sh: '${sha}' is not a commit SHA" >&2; exit 2 ;;
esac
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
if [ "${mode}" = main ]; then
	timeout="${CI_GREEN_TIMEOUT:-7200}"
else
	timeout="${CI_GREEN_TIMEOUT:-2700}"
fi
interval="${CI_GREEN_INTERVAL:-60}"
tries="${CI_GREEN_TRIES:-3}"
pause="${CI_GREEN_PAUSE:-5}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# gh_list <out-file> <api path under repos/OWNER/REPO/> <jq>: the listing,
# paginated, into out-file. Up to ${tries} tries, ${pause}s apart; each failed
# try prints a warning with gh's error, on stderr so that a caller in $(...)
# keeps only the listing.
gh_list() {
	try=1
	while :; do
		if gh api --paginate "repos/${repo}/$2" --jq "$3" >"$1" 2>"${tmp}/err"; then
			return 0
		fi
		echo "::warning::could not list ${2%%\?*} (try ${try} of ${tries}): $(tr '\n' ' ' <"${tmp}/err")" >&2
		[ "${try}" -lt "${tries}" ] || return 1
		sleep "${pause}"
		try=$((try + 1))
	done
}

# main-push mode. One line per ci.yml run of a push to main for the commit:
# "<id> <status> <conclusion> <url> <check suite id>".
main_push_runs() {
	gh_list "${tmp}/runs" \
		"actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=main&per_page=100" \
		'.workflow_runs[] | select(.path == ".github/workflows/ci.yml" and .event == "push" and .head_branch == "main" and .head_sha == "'"${sha}"'") | "\(.id) \(.status) \(.conclusion // "none") \(.html_url) \(.check_suite_id // "none")"' &&
		cat "${tmp}/runs"
}

# run_jobs <run id> <check suite id>: the latest attempt of each job of the
# run, "<conclusion><TAB><name>", into ${tmp}/jobs. Each of ${tries} rounds
# asks the jobs endpoint, then, if that fails, the run's check suite for its
# latest check runs (GitHub Actions makes one per job, named as the job).
run_jobs() {
	round=1
	while :; do
		if gh api --paginate "repos/${repo}/actions/runs/$1/jobs?filter=latest&per_page=100" \
			--jq '.jobs[] | "\(.conclusion // "none")\t\(.name)"' >"${tmp}/jobs" 2>"${tmp}/err"; then
			return 0
		fi
		echo "::warning::could not list the jobs of CI run $1 (try ${round} of ${tries}): $(tr '\n' ' ' <"${tmp}/err")"
		if [ "$2" != none ]; then
			if gh api --paginate "repos/${repo}/check-suites/$2/check-runs?filter=latest&per_page=100" \
				--jq '.check_runs[] | select(.app.slug == "github-actions") | "\(.conclusion // "none")\t\(.name)"' >"${tmp}/jobs" 2>"${tmp}/err"; then
				echo "listed CI run $1's jobs from the latest check runs of its check suite $2"
				return 0
			fi
			echo "::warning::could not list the check runs of CI run $1's check suite $2 (try ${round} of ${tries}): $(tr '\n' ' ' <"${tmp}/err")"
		fi
		[ "${round}" -lt "${tries}" ] || return 1
		sleep "${pause}"
		round=$((round + 1))
	done
}

# Reads run_jobs lines; prints nothing and succeeds when the run is a full
# pass, else prints why not. Every job passed or was skipped, and these
# passed, not skipped: each compatibility shard (at least one), the check
# that every compat test ran in a shard, the integration suite and ci-green.
full_pass() {
	awk -F '\t' '
		NF < 2 { next }
		{ jobs++ }
		$1 != "success" && $1 != "skipped" { why = why "\n  " $2 ": " $1 }
		index($2, "official SDK compatibility (") == 1 {
			shards++
			if ($1 == "skipped") why = why "\n  " $2 ": skipped"
		}
		$2 == "every compat test ran in a shard" && $1 == "success" { coverage = 1 }
		$2 == "integration (cluster)" && $1 == "success" { integration = 1 }
		$2 == "ci-green" && $1 == "success" { green = 1 }
		END {
			if (!jobs) why = why "\n  the run has no jobs"
			if (!shards) why = why "\n  no official SDK compatibility shard ran"
			if (!coverage) why = why "\n  every compat test ran in a shard: did not pass"
			if (!integration) why = why "\n  integration (cluster): did not pass"
			if (!green) why = why "\n  ci-green: did not pass"
			if (why != "") { printf "%s\n", substr(why, 2); exit 1 }
		}'
}

if [ "${mode}" = main ]; then
	waited=0
	while :; do
		pending=0
		reasons=""
		if runs="$(main_push_runs)"; then
			# Completed successful runs first: a pass on any attempt is enough.
			for id in $(printf '%s\n' "${runs}" | awk '$2 == "completed" && $3 == "success" { print $1 }'); do
				suite="$(printf '%s\n' "${runs}" | awk -v id="${id}" '$1 == id { print ($5 == "" ? "none" : $5); exit }')"
				if ! run_jobs "${id}" "${suite}"; then
					echo "::warning::could not list the jobs of CI run ${id}; retrying"
					pending=1
					continue
				fi
				jobs="$(cat "${tmp}/jobs")"
				if why="$(printf '%s\n' "${jobs}" | full_pass)"; then
					echo "main CI passed in full for ${sha}: $(printf '%s\n' "${runs}" | awk -v id="${id}" '$1 == id { print $4 }')"
					exit 0
				fi
				reasons="${reasons}run ${id}:
${why}
"
			done
			running="$(printf '%s\n' "${runs}" | awk 'NF && $2 != "completed"' | wc -l | tr -d ' ')"
			[ "${running}" -eq 0 ] || pending=1
			if [ "${pending}" -eq 0 ]; then
				if [ -z "${runs}" ]; then
					echo "::error::Refusing to release ${sha}: it has no CI run of its own push to main. A release needs main's full CI run (compat shards included) to have passed on this exact commit; a pull request's ci-green is not enough."
				else
					echo "::error::Refusing to release ${sha}: no CI run of its push to main passed in full, and none is still running. A run cancelled by a later push to main does not count."
					printf '%s\n' "${runs}" | awk '{ printf "  run %s %s: %s\n", $1, $3, $4 }'
					[ -z "${reasons}" ] || printf '%s' "${reasons}"
				fi
				exit 1
			fi
		else
			echo "::warning::could not list the CI runs on main for ${sha}; retrying"
		fi
		if [ "${waited}" -ge "${timeout}" ]; then
			echo "::error::Refusing to release ${sha}: main's CI run for this commit had not passed in full after ${waited}s. Re-run the release once it has."
			exit 1
		fi
		echo "main CI for ${sha} is still running; checking again in ${interval}s (${waited}s of ${timeout}s waited)"
		sleep "${interval}"
		waited=$((waited + interval))
	done
fi

# One line per ci-green check run created by GitHub Actions for the commit:
# "<status> <conclusion> <url>". Another app cannot pass for it by naming a
# check ci-green.
check_runs() {
	gh_list "${tmp}/checks" \
		"commits/${sha}/check-runs?check_name=ci-green&filter=all&per_page=100" \
		'.check_runs[] | select(.app.slug == "github-actions") | "\(.status) \(.conclusion // "none") \(.html_url)"' &&
		cat "${tmp}/checks"
}

# The status of each CI workflow run for the commit. ci-green waits on every
# other job, so while they run it may have no check run yet.
ci_runs() {
	gh_list "${tmp}/ci" \
		"actions/runs?head_sha=${sha}&per_page=100" \
		'.workflow_runs[] | select(.path == ".github/workflows/ci.yml") | .status' &&
		cat "${tmp}/ci"
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
