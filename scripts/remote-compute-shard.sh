#!/usr/bin/env bash
# remote-compute-shard.sh: the remotehosts shard's stand-in hosts and the
# tests that need them (#2209, #2259), for ci.yml's compat job and
# scripts/verify-local.sh alike. Every host is the Docker-in-Docker sshd
# stand-in scripts/remote-compute-standin.sh starts, on 127.0.0.1 only;
# the clouds are test/tools/byocfakes's local fakes. Nothing leaves the
# machine and nothing signs in to a cloud.
#
# Usage:
#   remote-compute-shard.sh hosts <dir> <state-dir> <instance> <log>
#       The stand-in tests of internal/remotecompute and cmd/cloudburrow
#       against two stand-ins that accept one bootstrap key, with the
#       pinned busybox image (CLOUDBURROW_TEST_REMOTE_IMAGE), into <log>:
#       every one of them must pass. Then the stand-in <instance> runs on,
#       its own key already authorized, and its variables printed for eval
#       (CLOUDBURROW_TEST_REMOTE_HOST, the `up --compute` target;
#       CLOUDBURROW_TEST_REMOTE_STANDIN; CLOUDBURROW_TEST_REMOTE_COMPUTE).
#   remote-compute-shard.sh walkthrough <dir> <cli> <log>
#       TestByocFirstRunWalkThrough: two more stand-ins (the AWS and the
#       Azure VM), byocfakes, an instance of its own on this machine's
#       Docker started with CLOUDBURROW_BYOC_FAKES, and the browser test,
#       into <log>; it must pass. The instance is stopped and deleted after.
#       The browser suite's variables (CLOUDBURROW_TEST_CHROME and the
#       rest) come from the caller's environment.
#   remote-compute-shard.sh cleanup <dir>
#       Removes every stand-in this script started, by container ID, and
#       ends byocfakes by its PID. Images are left to the caller, which
#       removes the ones it did not have before by ID.
#
# Ports 23022 to 23026 (the stand-ins' sshd) and 23200 (the walk-through
# instance's --port-base) on 127.0.0.1.
set -euo pipefail
# The stand-ins, fakes and the walk-through's instance are this machine's
# Docker's, whatever instance the caller's DOCKER_HOST names.
unset DOCKER_HOST DOCKER_CONTEXT

root=$(cd "$(dirname "$0")/.." && pwd)
cmd=${1:?usage: remote-compute-shard.sh hosts|walkthrough|cleanup <dir> ...}
dir=${2:?usage: remote-compute-shard.sh $cmd <dir> ...}
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)

# The image the stand-in tests run on the hosts: sh, nc, httpd, true and
# rm, and no VOLUME of its own (hostmount_standin_test.go counts the
# model container's mounts): busybox, pinned by its multi-platform index
# digest (dependencies.json components.realTools.busybox).
image="busybox:$(jq -er '.components.realTools.busybox.version' "$root/dependencies.json")@$(jq -er '.components.realTools.busybox.digest' "$root/dependencies.json")"

# standin <port> <key dir> [<state-dir> <instance>]: starts one, records its
# container for cleanup, and prints its variables.
standin() {
  local out
  out=$("$root/scripts/remote-compute-standin.sh" "$2" "$1" ${3:+"$3"} ${4:+"$4"})
  sed -n 's/^# container \([0-9a-f]*\), image .*/\1/p' <<< "$out" >> "$dir/containers"
  printf '%s\n' "$out"
}

# all_passed <log> <tags> <pattern> <package>: every test of the package
# that the pattern selects passed (go test exits 0 on a skip).
all_passed() {
  local log=$1 tags=$2 pattern=$3 pkg=$4 t bad=
  for t in $(cd "$root" && go test ${tags:+-tags="$tags"} -list "$pattern" "$pkg" | grep '^Test'); do
    grep -q -- "^--- PASS: $t (" "$log" ||
      { echo "$t did not pass: $(grep -- "^--- [A-Z]*: $t (" "$log" || echo 'no result')" >&2; bad=1; }
  done
  [ -z "$bad" ]
}

remove_standins() {
  local id
  [ -s "$dir/containers" ] || return 0
  while read -r id; do
    [ -n "$id" ] && docker rm -f -v "$id" >/dev/null 2>&1 || true
  done < "$dir/containers"
  : > "$dir/containers"
}

case "$cmd" in
  hosts)
    state=${3:?usage: remote-compute-shard.sh hosts <dir> <state-dir> <instance> <log>}
    instance=${4:?usage: remote-compute-shard.sh hosts <dir> <state-dir> <instance> <log>}
    log=${5:?usage: remote-compute-shard.sh hosts <dir> <state-dir> <instance> <log>}
    docker pull -q "$image" >/dev/null
    # Two hosts that accept the same bootstrap key, as one user's agent
    # signs in to both (TestStandInMoveBetweenTwoHosts).
    a=$(standin 23022 "$dir/key")
    standin 23023 "$dir/key" >/dev/null
    eval "$(grep '^export ' <<< "$a")"
    export CLOUDBURROW_TEST_REMOTE_HOST_2=ssh://cb@127.0.0.1:23023 CLOUDBURROW_TEST_REMOTE_IMAGE="$image"
    rc=0
    (cd "$root" && go test -count=1 -timeout 30m -v -run 'StandIn|Provisioned|SessionManagerTunnel' ./internal/remotecompute/) > "$log" 2>&1 || rc=1
    (cd "$root" && go test -count=1 -timeout 10m -v -run '^TestProvisionedCompute' ./cmd/cloudburrow/) >> "$log" 2>&1 || rc=1
    all_passed "$log" "" 'StandIn|Provisioned|SessionManagerTunnel' ./internal/remotecompute/ || rc=1
    all_passed "$log" "" '^TestProvisionedCompute' ./cmd/cloudburrow/ || rc=1
    echo "== stand-in tests: $(grep -c -- '^--- PASS' "$log") passed, $(grep -c -- '^--- FAIL' "$log") failed, $(grep -c -- '^--- SKIP' "$log") skipped" >&2
    remove_standins
    unset CLOUDBURROW_TEST_REMOTE_HOST CLOUDBURROW_TEST_REMOTE_HOST_2 CLOUDBURROW_TEST_REMOTE_BOOTSTRAP_KEY CLOUDBURROW_TEST_REMOTE_STANDIN
    [ "$rc" = 0 ] || { echo "the stand-in tests did not all pass (see $log)" >&2; exit 1; }
    # The instance's host, its own key authorized, so `up --compute`
    # signs in with that key alone.
    c=$(standin 23024 "$dir/key-instance" "$state" "$instance")
    grep '^export ' <<< "$c"
    echo "export CLOUDBURROW_TEST_REMOTE_COMPUTE='ssh://cb@127.0.0.1:23024'"
    ;;
  walkthrough)
    cli=${3:?usage: remote-compute-shard.sh walkthrough <dir> <cli> <log>}
    log=${4:?usage: remote-compute-shard.sh walkthrough <dir> <cli> <log>}
    standin 23025 "$dir/walk-key" >/dev/null
    standin 23026 "$dir/walk-key" >/dev/null
    (cd "$root" && go build -o "$dir/byocfakes" ./test/tools/byocfakes)
    "$dir/byocfakes" --dir "$dir/fakes" --aws-standin cb-remotehost-23025=127.0.0.1:23025 \
      --azure-standin cb-remotehost-23026=127.0.0.1:23026 > "$dir/fakes.log" 2>&1 &
    echo $! > "$dir/fakes.pid"
    for _ in $(seq 1 60); do [ -s "$dir/fakes/fakes.json" ] && break; sleep 1; done
    [ -s "$dir/fakes/fakes.json" ] || { echo "byocfakes wrote no fakes file: $(cat "$dir/fakes.log")" >&2; exit 1; }
    fakes="$dir/fakes/fakes.json"
    wstate="$dir/walk-state" name=cbwalk
    WALK=(--name "$name" --state-dir "$wstate" --port-base 23200)
    CLOUDBURROW_BYOC_FAKES=$fakes "$cli" up --detach --detach-timeout 15m "${WALK[@]}" > "$dir/walk-up.log" 2>&1 ||
      { tail -n 20 "$dir/walk-up.log" >&2; exit 1; }
    rc=0
    (
      eval "$("$root/scripts/compat-env.sh" --only CONSOLE,CONTROL,ADMIN_TOKEN,CLUSTER --cli "$cli" -- "${WALK[@]}" 2>/dev/null)"
      export CLOUDBURROW_TEST_BYOC_WALKTHROUGH=$fakes CLOUDBURROW_TEST_BYOC_INSTANCE_DIR=$wstate/$name \
        CLOUDBURROW_TEST_REMOTE_STANDIN=cb-remotehost-23025 CLOUDBURROW_TEST_REMOTE_STANDIN_2=cb-remotehost-23026
      cd "$root" && go test -tags=browser -count=1 -timeout 120m -v -run '^TestByocFirstRunWalkThrough$' ./test/browser/
    ) > "$log" 2>&1 || rc=1
    grep -q -- '^--- PASS: TestByocFirstRunWalkThrough (' "$log" || rc=1
    CLOUDBURROW_BYOC_FAKES=$fakes "$cli" stop "${WALK[@]}" >> "$dir/walk-up.log" 2>&1 || true
    CLOUDBURROW_BYOC_FAKES=$fakes "$cli" delete "${WALK[@]}" >> "$dir/walk-up.log" 2>&1 || true
    [ "$rc" = 0 ] || { echo "TestByocFirstRunWalkThrough did not pass: $(grep -- '^--- [A-Z]*: TestByocFirstRunWalkThrough' "$log" || echo 'no result')" >&2; exit 1; }
    ;;
  cleanup)
    if [ -s "$dir/fakes.pid" ]; then kill "$(cat "$dir/fakes.pid")" 2>/dev/null || true; rm -f "$dir/fakes.pid"; fi
    remove_standins
    ;;
  *) echo "remote-compute-shard.sh: unknown command $cmd (hosts, walkthrough or cleanup)" >&2; exit 2 ;;
esac
