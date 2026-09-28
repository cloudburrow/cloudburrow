#!/usr/bin/env bash
# verify-local.sh: ci.yml's compat job, shard by shard, on this machine, with
# a dated, machine-readable result (#705). GitHub's macOS runners cannot run
# the cluster, so this is how macOS (Docker Desktop) is verified: `up
# --detach`, the environment from scripts/compat-env.sh, the compat suite,
# the acceptance tests and the restart probes, then stop and delete.
#
# What each shard starts and runs is read from scripts/compat-shards.sh, the
# file ci.yml's compat job sources, so the two cannot drift. It needs no CI
# variable: it makes its own instances, each named verify-<time>-<shard> at
# --port-base (default 41000, clear of a default instance's 9000-9090), in a
# state directory of its own, so a running instance is never touched.
#
# Usage: scripts/verify-local.sh [options]
#
# Options:
#   --shards A,B,...  the shards to run, in order; default every one:
#                     storage,served,run,emulators,acceptance
#   --port-base N     the instances' port layout (default 41000)
#   --out DIR         where the result and logs go (default ./verify-local)
#   --no-build        use bin/cloudburrow as it is instead of `make build`
#   --no-sdk-suites   skip the Node.js and Python client suites
#   --no-browser      skip the console's headless-browser suite
#   --keep-images     keep the Docker images this run pulled or built
#
# The result is <out>/<UTC time>-<os>-<arch>.json, with each suite's pass,
# fail and skip counts and the failed tests, and the logs beside it. Exit 0
# when nothing failed, 1 otherwise, 2 on a usage error.
#
# Cleanup, on any exit: only what this run made. Its instances are stopped
# and deleted (their kind clusters are cloudburrow-verify-*), its state
# directory removed, and, unless --keep-images, the Docker images that were
# not present when it started removed by ID, never by name and never
# forced. No global prune, and no other kind cluster or instance is touched.
#
# It refuses to run with cloud credentials in the environment, as the
# harness does. Requires bash, jq, go, kind, docker and make.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^# Cleanup/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; }

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=scripts/compat-shards.sh
. scripts/compat-shards.sh

shards=${COMPAT_SHARDS// /,} port_base=41000 out=verify-local
build=1 sdk=1 browser=1 keep_images=""
while [ $# -gt 0 ]; do
  case "$1" in
    --shards) shards=${2:?--shards needs a list}; shift 2 ;;
    --shards=*) shards=${1#*=}; shift ;;
    --port-base) port_base=${2:?--port-base needs a port}; shift 2 ;;
    --port-base=*) port_base=${1#*=}; shift ;;
    --out) out=${2:?--out needs a directory}; shift 2 ;;
    --out=*) out=${1#*=}; shift ;;
    --no-build) build=""; shift ;;
    --no-sdk-suites) sdk=""; shift ;;
    --no-browser) browser=""; shift ;;
    --keep-images) keep_images=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "verify-local: unknown argument $1" >&2; usage; exit 2 ;;
  esac
done
case "$port_base" in
  ''|*[!0-9]*) echo "verify-local: --port-base must be a number" >&2; exit 2 ;;
esac
if [ "$port_base" -lt 1024 ] || [ "$port_base" -gt 65000 ]; then
  echo "verify-local: --port-base $port_base is outside 1024-65000" >&2; exit 2
fi
if [ "$port_base" -gt 8900 ] && [ "$port_base" -lt 9200 ]; then
  echo "verify-local: --port-base $port_base overlaps a default instance's 9000-9090 layout" >&2; exit 2
fi
IFS=', ' read -r -a SHARD_LIST <<< "$shards"
for s in "${SHARD_LIST[@]}"; do
  case " $COMPAT_SHARDS " in
    *" $s "*) ;;
    *) echo "verify-local: unknown shard $s (one of: $COMPAT_SHARDS)" >&2; exit 2 ;;
  esac
done

# The harness refuses these (test/compat/harness.go), and a run with them set
# could reach Google with a developer's own account; refuse before starting
# anything.
for v in GOOGLE_APPLICATION_CREDENTIALS GOOGLE_CLOUD_PROJECT GCLOUD_PROJECT GOOGLE_CREDENTIALS \
  GOOGLE_OAUTH_ACCESS_TOKEN CLOUDSDK_AUTH_ACCESS_TOKEN CLOUDSDK_AUTH_ACCESS_TOKEN_FILE \
  CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE CLOUDSDK_CORE_PROJECT; do
  if [ -n "${!v:-}" ]; then
    echo "verify-local: $v is set; unset it (the compat harness refuses cloud credentials in the environment)" >&2
    exit 2
  fi
done
for c in jq go kind docker make; do
  command -v "$c" >/dev/null || { echo "verify-local: $c is required" >&2; exit 2; }
done
docker info >/dev/null 2>&1 || { echo "verify-local: docker is not running" >&2; exit 2; }

mkdir -p "$out"
out=$(cd "$out" && pwd)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
RUN_ID=$(date -u +%y%m%d%H%M%S)
OS=$(go env GOOS) ARCH=$(go env GOARCH)
LOGS="$out/$STAMP-$OS-$ARCH"
RESULT="$LOGS.json"
mkdir -p "$LOGS"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/cb-verify.XXXXXX")
STATE="$WORK/state"
mkdir -p "$STATE"
CLI="$ROOT/bin/cloudburrow"
CREATED=()   # instance names this run started
IMAGES_BEFORE="$WORK/images-before"
docker images -q --no-trunc | sort -u > "$IMAGES_BEFORE"

log() { echo "== verify-local: $*" >&2; }

# cleanup removes only what this run made; it runs on every exit.
cleanup() {
  local rc=$? n cluster id
  trap - EXIT INT TERM
  set +e
  for n in ${CREATED[@]+"${CREATED[@]}"}; do
    case "$n" in verify-*) ;; *) continue ;; esac
    if [ -e "$STATE/$n/up.json" ]; then
      "$CLI" stop --name "$n" --state-dir "$STATE" >/dev/null 2>&1
      pid=$(jq -r '.pid // empty' "$STATE/$n/up.json" 2>/dev/null)
      [ -z "$pid" ] || kill "$pid" 2>/dev/null
    fi
    "$CLI" delete --name "$n" --state-dir "$STATE" >/dev/null 2>&1
    cluster="cloudburrow-$n"
    if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
      kind delete cluster --name "$cluster" >/dev/null 2>&1 || log "could not delete kind cluster $cluster"
    fi
  done
  if [ -z "$keep_images" ]; then
    docker images -q --no-trunc | sort -u | comm -13 "$IMAGES_BEFORE" - | while read -r id; do
      docker rmi "$id" >/dev/null 2>&1 || log "kept image $id (in use, or shared with a tag present before)"
    done
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "$build" ]; then
  log "make build"
  make build > "$LOGS/build.log" 2>&1 || { tail -n 20 "$LOGS/build.log" >&2; exit 1; }
fi
[ -x "$CLI" ] || { echo "verify-local: no $CLI; run make build" >&2; exit 2; }
# shellcheck source=.github/scripts/retry-flaky.sh
. .github/scripts/retry-flaky.sh

# Results, as JSON lines in $WORK, one file per shard.
SUITES="" CHECKS=""
suite_json() {  # <name> <passed> <failed> <skipped> <failed tests, newline-separated> [note]
  jq -cn --arg name "$1" --argjson p "$2" --argjson f "$3" --argjson s "$4" --arg failed "$5" --arg note "${6:-}" \
    '{name: $name, passed: $p, failed: $f, skipped: $s,
      failed_tests: ($failed | split("\n") | map(select(. != "")))}
     + (if $note == "" then {} else {note: $note} end)' >> "$SUITES"
}
check() {  # <name> <pass|fail|skip> [detail]
  jq -cn --arg name "$1" --arg r "$2" --arg d "${3:-}" \
    '{name: $name, result: $r} + (if $d == "" then {} else {detail: $d} end)' >> "$CHECKS"
  [ "$2" != fail ] || log "check failed: $1${3:+: $3}"
}
# A go test -v log's top-level results; RETRIED lines name retries.
# With "strict", a skip is a failure, as CI's all_passed makes it for the
# acceptance tests and the browser suite: each skips on a missing variable
# or tool, and go test exits 0 on a skip.
go_suite() {  # <name> <log> [note] [strict]
  local p f s failed retried note=${3:-}
  p=$(grep -c '^--- PASS: ' "$2" || true)
  f=$(grep -c '^--- FAIL: ' "$2" || true)
  s=$(grep -c '^--- SKIP: ' "$2" || true)
  failed=$(grep '^--- FAIL: ' "$2" | awk '{print $3}' | sort -u || true)
  if [ "${4:-}" = strict ] && [ "$s" -gt 0 ]; then
    f=$((f + s))
    failed=$(printf '%s\n' "$failed"; grep '^--- SKIP: ' "$2" | awk '{print $3 " (skipped; CI requires a pass)"}')
    s=0
  fi
  retried=$(grep -h '^RETRIED:' "$2" | sed 's/RETRIED: //' | tr '\n' ' ' || true)
  [ -z "$retried" ] || note="${note:+$note; }retried once: $retried"
  # A package-level failure (a timeout or panic) names no test. A retried
  # run's first FAIL line is settled by settle_retries instead.
  if [ "$f" -eq 0 ] && [ -z "$retried" ] && grep -qE '^(FAIL[[:space:]]|panic: )' "$2"; then
    f=1 failed="(package-level failure: $(grep -E '^(FAIL[[:space:]]|panic: )' "$2" | head -1))"
  fi
  suite_json "$1" "$p" "$f" "$s" "$failed" "$note"
}
# A retried go test run's log: a test that failed and then passed on its
# retry counts as passed, as in CI.
settle_retries() {  # <log>
  local t
  grep '^--- FAIL: ' "$1" | awk '{print $3}' | sort -u > "$1.failed" || true
  while read -r t; do
    if grep -q -- "^--- PASS: $t (" "$1"; then
      sed -i.bak "/^--- FAIL: $t (/d" "$1" && rm -f "$1.bak"
    fi
  done < "$1.failed"
  rm -f "$1.failed"
}

run_shard() {
  local shard=$1 name="verify-$RUN_ID-$1" dir="$LOGS/$1" t0 rc
  t0=$(date +%s)
  mkdir -p "$dir"
  SUITES="$WORK/$shard.suites" CHECKS="$WORK/$shard.checks"
  : > "$SUITES"; : > "$CHECKS"
  compat_shard "$shard"
  # The hooks write where RUNNER_TEMP names; here, the shard's own directory.
  export RUNNER_TEMP="$WORK/$shard"
  mkdir -p "$RUNNER_TEMP"
  local CB=(--name "$name" --state-dir "$STATE" ${FIXTURES[@]+"${FIXTURES[@]}"} --port-base "$port_base"
    --port-control 0 --port-storage 0 --port-pubsub 0 --port-run 0 --port-memorystore 0 --port-cloudsql-mysql 0 --port-cloudsql 0
    --services "$SERVICES")
  local RUNTIME="$STATE/$name/up.json" UPLOG="$STATE/$name/up.log" PID
  CREATED+=("$name")
  CLUSTER_HOST=""
  log "$shard: up --detach $name ($SERVICES) at --port-base $port_base"
  if ! "$CLI" up --detach --detach-timeout 15m "${CB[@]}" > "$dir/up.out" 2>&1; then
    check "up --detach" fail "$(tail -n 5 "$dir/up.out" | tr '\n' ' ')"
    cp "$UPLOG" "$dir/up.log" 2>/dev/null || true
    shard_done "$shard" "$name" "$t0"
    return
  fi
  check "up --detach" pass
  # How pods reach the CLI-hosted services: host.docker.internal on Docker
  # Desktop, the kind gateway through the CLI's relay on Docker Engine (#575).
  CLUSTER_HOST=$(grep -o 'pods reach .*' "$UPLOG" | head -1 || true)
  PID=$(jq -r .pid "$RUNTIME")
  "$CLI" up --detach "${CB[@]}" > "$dir/detach2.log" 2>&1 || true
  if grep -q 'already running' "$dir/detach2.log" && [ "$(jq -r .pid "$RUNTIME")" = "$PID" ]; then
    check "a second up --detach leaves the instance alone" pass
  else
    check "a second up --detach leaves the instance alone" fail
  fi
  if "$CLI" wait --timeout 1m "${CB[@]}" > "$dir/wait.log" 2>&1; then check wait pass; else check wait fail; fi
  if [ "$shard" = storage ]; then
    local seeded ready
    seeded=$(grep -n 'seeded .* from .*seed.json' "$UPLOG" | head -1 | cut -d: -f1 || true)
    ready=$(grep -n 'press Ctrl-C to stop' "$UPLOG" | head -1 | cut -d: -f1 || true)
    if [ -n "$seeded" ] && [ -n "$ready" ] && [ "$seeded" -lt "$ready" ]; then
      check "the seed file is applied before the ready line" pass
    else
      check "the seed file is applied before the ready line" fail
    fi
  fi
  if [ "$shard" = emulators ]; then
    # CI checks BigQuery's default port; here it moved with --port-base.
    "$CLI" status "${CB[@]}" > "$dir/status.log" 2>&1 || true
    if grep -q "^  bigquery  *127.0.0.1:$((port_base + 14))" "$dir/status.log"; then
      check "status names the BigQuery endpoint" pass
    else
      check "status names the BigQuery endpoint" fail
    fi
  fi

  if ! "$CLI" env "${CB[@]}" --format json > "$WORK/env.json" 2> "$dir/env.err"; then
    check "cloudburrow env" fail "$(tail -n 3 "$dir/env.err" | tr '\n' ' ')"
    shard_done "$shard" "$name" "$t0"
    return
  fi
  # Not --strict, unlike CI: a tool CI installs (the pinned gcloud, OpenTofu)
  # may be missing here, and its tests then skip; the variables left unset
  # are recorded.
  local COMPAT_ENV
  COMPAT_ENV=$(scripts/compat-env.sh --only "$TEST_VARS" --env-json "$WORK/env.json" --cli "$CLI" -- "${CB[@]}" 2> "$dir/compat-env.err" || true)
  eval "$COMPAT_ENV"
  local unset_vars
  unset_vars=$(sed -n 's/^compat-env: not set, so the tests that need them skip: //p' "$dir/compat-env.err")
  [ -z "$unset_vars" ] || check "compat-env.sh sets every shard variable" skip "unset: $unset_vars"
  if [ -s "${CLOUDBURROW_TEST_KUBECONFIG:-}" ] && kind get clusters | grep -qx "${CLOUDBURROW_TEST_CLUSTER:-}"; then
    check "the instance's kubeconfig and kind cluster" pass
  else
    check "the instance's kubeconfig and kind cluster" fail
  fi

  if [ "$shard" = acceptance ]; then
    local ACCEPTANCE_ENV=() line
    while IFS= read -r line; do [ -z "$line" ] || ACCEPTANCE_ENV+=("$line"); done < <(
      scripts/compat-env.sh --plain --only STORAGE,PUBSUB,RUN --env-json "$WORK/env.json" --cli "$CLI" -- "${CB[@]}")
    ACCEPTANCE_ENV+=(
      "CLOUDBURROW_TEST_STORAGE_INCLUSTER=$(grep -oE 'storage +host [^ ]+ +in-cluster [^ ]+' "$UPLOG" | head -1 | awk '{print $NF}' || true)"
      "CLOUDBURROW_TEST_INGRESS=$(jq -r '.CLOUDBURROW_INGRESS // empty | sub("^.*:"; "")' "$WORK/env.json")"
    )
    log "$shard: test/e2e and test/k8s"
    env "${ACCEPTANCE_ENV[@]}" go test -tags=e2e -count=1 -timeout 20m -v ./test/e2e/... > "$dir/e2e.log" 2>&1 || true
    go_suite e2e "$dir/e2e.log" "" strict
    env "${ACCEPTANCE_ENV[@]}" go test -tags=integration -count=1 -timeout 20m -v ./test/k8s/... > "$dir/k8s.log" 2>&1 || true
    go_suite k8s "$dir/k8s.log" "" strict
  else
    log "$shard: compat suite"
    rc=0
    go test -tags=compat -count=1 -timeout 25m -v ./test/compat/... > "$dir/compat.log" 2>&1 || rc=$?
    if [ "$rc" -ne 0 ]; then
      retry_flaky "$dir/compat.log" go test -tags=compat -count=1 -timeout 10m -v ./test/compat/... > "$dir/retry.out" 2>&1 || true
      settle_retries "$dir/compat.log"
    fi
    go_suite compat "$dir/compat.log"
  fi

  if [ -n "$sdk" ] && { [ "$shard" = storage ] || [ "$shard" = emulators ]; }; then
    node_suite "$shard" "$dir" "${CB[*]}"
  fi
  if [ -n "$sdk" ] && [ "$shard" = storage ]; then
    log "$shard: Python suite"
    make compat-python CLOUDBURROW_ARGS="${CB[*]}" > "$dir/python.log" 2>&1 || true
    local pp pf ps
    pp=$(grep -cE ' PASSED( |$)' "$dir/python.log" || true)
    pf=$(grep -cE ' (FAILED|ERROR)( |$)' "$dir/python.log" || true)
    ps=$(grep -cE ' SKIPPED( |$)' "$dir/python.log" || true)
    if [ "$pp" -eq 0 ] && [ "$pf" -eq 0 ]; then pf=1; fi
    suite_json python "$pp" "$pf" "$ps" "$(grep -E ' (FAILED|ERROR)( |$)' "$dir/python.log" | awk '{print $1}' || true)" "$(python3 --version 2>&1)"
  fi
  if [ "$shard" = storage ]; then
    "$CLI" logs "${CB[@]}" --service pubsub --tail 20 > "$dir/logs.txt" 2>&1 || true
    if [ -s "$dir/logs.txt" ] && [ "$(wc -l < "$dir/logs.txt")" -le 20 ]; then
      check "logs --tail 20" pass
    else
      check "logs --tail 20" fail
    fi
    local fpid code=0
    "$CLI" logs "${CB[@]}" --service pubsub --follow > "$dir/follow.txt" 2>&1 &
    fpid=$!
    sleep 5
    kill -INT "$fpid" 2>/dev/null || true
    wait "$fpid" || code=$?
    if [ "$code" = 130 ]; then check "logs --follow exits 130 on SIGINT" pass; else check "logs --follow exits 130 on SIGINT" fail "exit $code"; fi
  fi

  if [ -n "$SETUP_TESTS" ]; then
    compat_setup_exports "$shard" "$RUNNER_TEMP"
    go test -tags=compat -count=1 -v -run "^($(echo "$SETUP_TESTS" | tr ' ' '|'))$" ./test/compat/ > "$dir/probe-setup.log" 2>&1 || true
    go_suite "restart probe setup" "$dir/probe-setup.log"
  fi

  "$CLI" stop "${CB[@]}" > "$dir/stop.log" 2>&1 || true
  if kill -0 "$PID" 2>/dev/null; then check "stop ends up" fail "pid $PID survived"; else check "stop ends up" pass; fi
  if [ "$shard" = storage ]; then
    if grep -q '"cloudburrow-hook-bucket"' "$RUNNER_TEMP/cb-shutdown-hook.json" 2>/dev/null; then
      check "the shutdown.d hook reaches storage before the cluster stops" pass
    else
      check "the shutdown.d hook reaches storage before the cluster stops" fail
    fi
  fi
  if [ ! -e "$RUNTIME" ]; then check "stop removes the runtime file" pass; else check "stop removes the runtime file" fail; fi

  if [ -n "$PROBE_TESTS" ]; then
    local PROBE_RUN expect
    PROBE_RUN="^($(echo "$PROBE_TESTS" | tr ' ' '|'))$"
    : > "$dir/restart.log"
    for expect in present absent; do
      local mode=()
      [ "$expect" = present ] || mode=(--mode ephemeral)
      log "$shard: restart probes, $expect"
      if ! "$CLI" up --detach --detach-timeout 15m "${CB[@]}" ${mode[@]+"${mode[@]}"} > "$dir/up-$expect.out" 2>&1; then
        check "up --detach for the $expect probes" fail "$(tail -n 5 "$dir/up-$expect.out" | tr '\n' ' ')"
        continue
      fi
      "$CLI" env "${CB[@]}" ${mode[@]+"${mode[@]}"} --format json > "$WORK/env-restart.json"
      local PENV=() line
      while IFS= read -r line; do [ -z "$line" ] || PENV+=("$line"); done < <(
        scripts/compat-env.sh --plain --only "$PROBE_VARS" --env-json "$WORK/env-restart.json" --cli "$CLI" -- "${CB[@]}"
        compat_probe_expect "$expect" "$RUNNER_TEMP")
      env "${PENV[@]}" go test -tags=compat -count=1 -v -run "$PROBE_RUN" ./test/compat/ > "$dir/probe-$expect.log" 2>&1 || true
      if grep -q -- '^--- FAIL: ' "$dir/probe-$expect.log"; then
        # A subshell, not env(1): retry_flaky is a shell function.
        (
          for line in "${PENV[@]}"; do export "${line?}"; done
          retry_flaky "$dir/probe-$expect.log" go test -tags=compat -count=1 -v ./test/compat/ > "$dir/probe-$expect.retry" 2>&1
        ) || true
        settle_retries "$dir/probe-$expect.log"
      fi
      go_suite "restart probes ($expect)" "$dir/probe-$expect.log"
      "$CLI" stop "${CB[@]}" ${mode[@]+"${mode[@]}"} >> "$dir/stop.log" 2>&1 || true
    done
  fi

  if [ -n "$browser" ]; then
    case " $COMPAT_BROWSER_SHARDS " in
      *" $shard "*) browser_suite "$shard" "$dir" "${CB[@]}" ;;
    esac
  fi
  cp "$UPLOG" "$dir/up.log" 2>/dev/null || true
  shard_done "$shard" "$name" "$t0"
}

node_suite() {  # <shard> <dir> <flags>
  local shard=$1 dir=$2 args=$3 np nf ns
  if ! command -v node >/dev/null || ! command -v npm >/dev/null; then
    suite_json node 0 0 0 "" "not run: no node or npm"
    return
  fi
  log "$shard: Node.js suite"
  if [ "$shard" = emulators ]; then
    NO_COLOR=1 make compat-node CLOUDBURROW_ARGS="$args" COMPAT_NODE_TESTS="firestore.test.mjs examples.test.mjs" \
      COMPAT_NODE_FLAGS=--test-name-pattern=Firestore > "$dir/node.log" 2>&1 || true
  else
    NO_COLOR=1 make compat-node CLOUDBURROW_ARGS="$args" COMPAT_NODE_FLAGS=--test-skip-pattern=Firestore > "$dir/node.log" 2>&1 || true
  fi
  np=$(sed -n 's/^ℹ pass //p' "$dir/node.log" | tail -1)
  nf=$(sed -n 's/^ℹ fail //p' "$dir/node.log" | tail -1)
  ns=$(sed -n 's/^ℹ skipped //p' "$dir/node.log" | tail -1)
  # No summary means the suite never ran: a failure.
  suite_json node "${np:-0}" "${nf:-1}" "${ns:-0}" "$(grep '^✖ ' "$dir/node.log" | sed 's/^✖ //; s/ ([0-9.]*ms)$//' | sort -u || true)" "$(node --version)"
}

browser_suite() {  # <shard> <dir> <flags...>
  local shard=$1 dir=$2; shift 2
  local chrome="" select pattern
  # As in CI: the KMS and Cloud Run screens' tests in the run shard, the
  # BigQuery, Firestore and Datastore screens' in the emulators shard, the
  # rest in storage.
  case "$shard" in
    run) select=-run pattern="$COMPAT_BROWSER_RUN_SHARD_TESTS" ;;
    emulators) select=-run pattern="$COMPAT_BROWSER_EMULATORS_SHARD_TESTS" ;;
    *) select=-skip pattern="$COMPAT_BROWSER_RUN_SHARD_TESTS|$COMPAT_BROWSER_EMULATORS_SHARD_TESTS" ;;
  esac
  for c in "${CLOUDBURROW_TEST_CHROME:-}" "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "$(command -v google-chrome || true)" "$(command -v google-chrome-stable || true)" "$(command -v chromium || true)"; do
    if [ -n "$c" ] && [ -x "$c" ]; then chrome=$c; break; fi
  done
  if [ -z "$chrome" ]; then
    suite_json browser 0 0 0 "" "not run: no Chrome found (set CLOUDBURROW_TEST_CHROME)"
    return
  fi
  log "$shard: console in headless Chrome"
  if ! "$CLI" up --detach --detach-timeout 15m "$@" > "$dir/up-browser.out" 2>&1; then
    check "up --detach for the browser suite" fail "$(grep -E '^ *cloudburrow: ' "$dir/up-browser.out" | sed 's/^ *//' | head -2 | tr '\n' ' ')"
    return
  fi
  local BROWSER_ENV
  BROWSER_ENV=$(scripts/compat-env.sh --only CONSOLE,CONTROL,ADMIN_TOKEN,CLUSTER --cli "$CLI" -- "$@" 2>/dev/null)
  (
    eval "$BROWSER_ENV"
    export CLOUDBURROW_TEST_CHROME="$chrome" CLOUDBURROW_TEST_SCREENSHOTS="$dir/browser-screenshots"
    go test -tags=browser -count=1 -timeout 10m -v "$select" "$pattern" ./test/browser/ > "$dir/browser.log" 2>&1 || true
  )
  go_suite browser "$dir/browser.log" "$("$chrome" --version 2>/dev/null || echo chrome)" strict
  "$CLI" stop "$@" >> "$dir/stop.log" 2>&1 || true
}

# shard_done writes the shard's result and deletes its instance.
shard_done() {  # <shard> <name> <t0>
  local shard=$1 name=$2 t0=$3
  "$CLI" stop --name "$name" --state-dir "$STATE" >/dev/null 2>&1 || true
  "$CLI" delete --name "$name" --state-dir "$STATE" > "$LOGS/$shard/delete.log" 2>&1 || true
  if kind get clusters 2>/dev/null | grep -qx "cloudburrow-$name"; then
    kind delete cluster --name "cloudburrow-$name" >/dev/null 2>&1 || true
  fi
  rm -rf "${STATE:?}/$name"
  jq -n --arg shard "$shard" --arg name "$name" --arg services "$SERVICES" --arg host "$CLUSTER_HOST" \
    --argjson secs "$(( $(date +%s) - t0 ))" \
    --slurpfile suites "$SUITES" --slurpfile checks "$CHECKS" \
    '{shard: $shard, instance: $name, services: ($services | split(",")), cluster_host: $host, duration_seconds: $secs,
      suites: $suites, checks: $checks}
     | .passed = ([.suites[].passed] | add // 0)
     | .failed = ([.suites[].failed] | add // 0) + ([.checks[] | select(.result == "fail")] | length)
     | .skipped = ([.suites[].skipped] | add // 0)
     | .failed_tests = ([.suites[] | .name as $s | .failed_tests[] | "\($s): \(.)"] + [.checks[] | select(.result == "fail") | "check: \(.name)"])
     | .result = (if .failed == 0 then "pass" else "fail" end)' > "$WORK/$shard.json"
  log "$shard: $(jq -r '"\(.result): \(.passed) passed, \(.failed) failed, \(.skipped) skipped in \(.duration_seconds)s"' "$WORK/$shard.json")"
  # Unset the shard's CLOUDBURROW_TEST_* so the next shard starts clean.
  local v
  for v in $(env | sed -n 's/^\(CLOUDBURROW_TEST_[A-Z0-9_]*\)=.*/\1/p'); do unset "$v"; done
}

tool_version() { command -v "$1" >/dev/null 2>&1 && "$@" 2>/dev/null | head -1 || echo ""; }
# shellcheck disable=SC1091
os_version() {
  if [ "$OS" = darwin ]; then echo "macOS $(sw_vers -productVersion) ($(sw_vers -buildVersion))"
  elif [ -r /etc/os-release ]; then (. /etc/os-release && echo "${PRETTY_NAME:-linux}")
  else uname -sr; fi
}

T0=$(date +%s)
for s in "${SHARD_LIST[@]}"; do
  run_shard "$s"
done

DOCKER_JSON=$(docker info --format '{{json .}}' | jq -c '{operating_system: .OperatingSystem, server_version: .ServerVersion,
  architecture: .Architecture, kernel: .KernelVersion, cpus: .NCPU, memory_bytes: .MemTotal}')
jq -n --arg date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg os "$OS" --arg arch "$ARCH" --arg osv "$(os_version)" \
  --argjson docker "$DOCKER_JSON" \
  --arg cli "$("$CLI" version)" --arg cliv "$("$CLI" version -short)" \
  --arg commit "$(git rev-parse HEAD 2>/dev/null || echo unknown)" \
  --arg dirty "$(git status --porcelain --untracked-files=no 2>/dev/null | head -1)" \
  --argjson port_base "$port_base" --argjson secs "$(( $(date +%s) - T0 ))" \
  --arg go "$(go version)" --arg kind "$(tool_version kind version)" --arg kubectl "$(tool_version kubectl version --client)" \
  --arg node "$(tool_version node --version)" --arg python "$(tool_version python3 --version)" \
  --arg terraform "$(tool_version terraform version)" --arg tofu "$(tool_version tofu version)" \
  --arg gcloud "$(tool_version gcloud version)" \
  --slurpfile shards <(for s in "${SHARD_LIST[@]}"; do cat "$WORK/$s.json"; done) \
  '{schema: 1, date: $date, host: {os: $os, arch: $arch, os_version: $osv}, docker: $docker,
    cli: {version: $cliv, detail: $cli, git_commit: $commit, tracked_changes: ($dirty != "")},
    tools: {go: $go, kind: $kind, kubectl: $kubectl, node: $node, python: $python, terraform: $terraform, tofu: $tofu, gcloud: $gcloud},
    port_base: $port_base, duration_seconds: $secs, shards: $shards}
   | .passed = ([.shards[].passed] | add // 0)
   | .failed = ([.shards[].failed] | add // 0)
   | .skipped = ([.shards[].skipped] | add // 0)
   | .failed_tests = [.shards[] | .shard as $s | .failed_tests[] | "\($s)/\(.)"]
   | .result = (if .failed == 0 then "pass" else "fail" end)' > "$RESULT"
log "result: $RESULT"
jq -r '"\(.result): \(.passed) passed, \(.failed) failed, \(.skipped) skipped", (.failed_tests[] | "  failed: \(.)")' "$RESULT" >&2
[ "$(jq -r .result "$RESULT")" = pass ]
