# shellcheck shell=bash
# retry_flaky, sourced by the compat job (.github/workflows/ci.yml).
#
# Called after `go test` exited non-zero, with the log it wrote and the
# command to re-run. A known-flaky backend restart test that failed is re-run
# once, by name, and the run fails only if it fails again (#566). Only the
# allow-list is retried: a regression anywhere else fails first time, and
# every retry is named in the log and the summary, never silent: each one
# is a ::warning:: annotation on the run page and, when $GITHUB_STEP_SUMMARY
# is set, a line of the job summary with the retry's outcome (#704).
#
# A non-zero exit with no top-level "--- FAIL:" line is a package-level
# failure: the -timeout panic, a panic outside a test, os.Exit in a helper,
# a TestMain or build failure. There is nothing to retry and it is not a
# pass: returning 0 there let a timed-out shard report green (#675).
retry_flaky() {  # <log> <command...>: re-runs the allow-listed failures in <log>, appending to it
  local log="$1"; shift
  # The allow-list. docs/compatibility.md ("Retried once") lists the same
  # tests, and test/repo fails when the two differ (#704). Removal rule: an
  # entry is taken off once 20 consecutive CI runs on main have passed with
  # no retry of it, counted from the ::warning:: annotations; a test is never
  # added without an entry in that doc section.
  local allow='^Test(MemorystoreAcrossRestart|SpannerStateDoesNotSurviveARestart|HostEndpointSurvivesABackendRestart|DatastoreSurvivesAPodRestart|DatastoreAcrossRestart)$'
  local failed retry
  failed=$(grep -- '^--- FAIL: ' "$log" | awk '{print $3}' | sort -u)
  if [ -z "$failed" ]; then
    echo "== failed with no test named: a package-level failure (a timeout, a panic outside a test, or a build or TestMain failure); not retried"
    grep -E '^(panic: |FAIL[[:space:]]|--- FAIL|ok[[:space:]])' "$log" | tail -n 5 || true
    return 1
  fi
  retry=$(printf '%s\n' "$failed" | grep -E "$allow" || true)
  if [ "$(printf '%s\n' "$failed" | sort)" != "$(printf '%s\n' "$retry" | sort)" ]; then
    echo "== failed, not retried (outside the flaky allow-list): $(printf '%s\n' "$failed" | grep -vE "$allow" | tr '\n' ' ')"
    return 1
  fi
  echo "== RETRIED once (known flaky): $(echo "$retry" | tr '\n' ' ')"
  echo "RETRIED: $(echo "$retry" | tr '\n' ' ')" >> "$log"
  local t rc=0 outcome
  for t in $retry; do
    echo "::warning title=Flaky test retried::$t failed and was re-run once (retry-once allow-list, docs/compatibility.md)"
  done
  "$@" -run "^($(echo "$retry" | paste -sd'|' -))$" | tee -a "$log" || rc=$?
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    for t in $retry; do
      if [ "$rc" -eq 0 ] || grep -q -- "^--- PASS: $t (" "$log"; then outcome="passed on the retry"; else outcome="failed again"; fi
      echo "- Retried once (known flaky): \`$t\`, $outcome" >> "$GITHUB_STEP_SUMMARY"
    done
  fi
  return "$rc"
}
