#!/bin/sh
# footprint.sh — measure cold start, warm start and steady-state memory (#312).
#
# For each profile it creates a fresh named instance, times `up --detach`
# from nothing (cold), stops it and times `up --detach` again (warm), then
# reads memory twice: the kind node container's own usage from `docker
# stats`, and the pods' working set from the kubelet summary API. It deletes
# only the instance it created, and nothing else.
#
# Usage: scripts/footprint.sh [out.json]    (needs docker, kind, kubectl, jq)
set -eu

BIN=${BIN:-./bin/cloudburrow}
OUT=${1:-footprint.json}
STATE=$(mktemp -d)
PARTS=$(mktemp)
trap 'rm -rf "$STATE" "$PARTS"' EXIT

DEFAULT_SERVICES="storage,pubsub,tasks,run,secretmanager"
ALL_SERVICES="storage,pubsub,tasks,run,secretmanager,firestore,datastore,bigtable,spanner,cloudsql,bigquery,memorystore,cloudsql-mysql,scheduler"

now() { date +%s; }

measure() {
  profile=$1
  services=$2
  name="footprint-$profile"
  set -- --name "$name" --state-dir "$STATE" --services "$services" \
    --port-control 0 --port-storage 0 --port-pubsub 0 --port-tasks 0 --port-run 0 \
    --port-secrets 0 --port-console 0 --port-metadata 0 \
    --port-firestore 0 --port-datastore 0 --port-bigtable 0 --port-spanner 0 \
    --port-bigquery 0 --port-bigquery-storage 0 --port-memorystore 0 --port-cloudsql-mysql 0
  "$BIN" delete "$@" >/dev/null 2>&1 || true

  t0=$(now)
  "$BIN" up --detach --detach-timeout 20m "$@" >&2
  t1=$(now)
  sleep 30 # let the pods settle before measuring steady state
  node="cloudburrow-$name-control-plane"
  kc="$STATE/$name/kubeconfig"
  node_mib=$(docker stats --no-stream --format '{{.MemUsage}}' "$node" |
    awk '{v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v); if(u=="GiB")v*=1024; if(u=="KiB")v/=1024; printf "%.0f", v}')
  pods_mib=$(kubectl --kubeconfig "$kc" get --raw "/api/v1/nodes/$node/proxy/stats/summary" |
    jq '[.pods[].memory.workingSetBytes // 0] | add / 1048576 | floor')
  "$BIN" stop "$@" >&2
  t2=$(now)
  "$BIN" up --detach --detach-timeout 20m "$@" >&2
  t3=$(now)
  "$BIN" delete "$@" >&2

  jq -n --arg p "$profile" --arg s "$services" --argjson cold "$((t1 - t0))" \
    --argjson warm "$((t3 - t2))" --argjson node "$node_mib" --argjson pods "$pods_mib" \
    '{profile:$p, services:($s|split(",")), cold_start_s:$cold, warm_start_s:$warm,
      memory_mib:{node_container:$node, pods_working_set:$pods}}' >> "$PARTS"
}

measure default "$DEFAULT_SERVICES"
measure all "$ALL_SERVICES"

host=$(jq -n --arg os "$(uname -sm)" --arg cpus "$(docker info --format '{{.NCPU}}')" \
  --arg mem "$(docker info --format '{{.MemTotal}}')" --arg date "$(date -u +%Y-%m-%d)" \
  --arg runner "${RUNNER_NAME:-local}" \
  '{os:$os, docker_cpus:($cpus|tonumber), docker_memory_mib:(($mem|tonumber)/1048576|floor), date:$date, runner:$runner}')
jq -s --argjson host "$host" '{host:$host, profiles:.}' "$PARTS" > "$OUT"
cat "$OUT"
