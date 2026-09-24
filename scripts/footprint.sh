#!/usr/bin/env bash
# footprint.sh — measure cold start, warm start and steady-state memory (#312).
#
# For each profile it creates a fresh named instance, times `up --detach`
# from nothing (cold), stops it and times `up --detach` again (warm), then
# reads memory twice: the kind node container's own usage from `docker
# stats`, and the pods' working set from the kubelet summary API. It deletes
# only the instance it created, and nothing else.
#
# Usage: scripts/footprint.sh [out.json]    (needs docker, kind, kubectl, jq)
set -euo pipefail

BIN=${BIN:-./bin/cloudburrow}
OUT=${1:-footprint.json}
STATE=$(mktemp -d)
trap 'rm -rf "$STATE"' EXIT

declare -A PROFILES=(
  [default]="storage,pubsub,tasks,run,secretmanager"
  [all]="storage,pubsub,tasks,run,secretmanager,firestore,datastore,bigtable,spanner,cloudsql,bigquery,memorystore,cloudsql-mysql,scheduler,logging"
)

now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }

measure() {
  local profile=$1 services=$2 name="footprint-$1"
  local flags=(--name "$name" --state-dir "$STATE" --services "$services"
    --port-control 0 --port-storage 0 --port-pubsub 0 --port-tasks 0 --port-run 0
    --port-secrets 0 --port-console 0 --port-metadata 0
    --port-firestore 0 --port-datastore 0 --port-bigtable 0 --port-spanner 0
    --port-bigquery 0 --port-bigquery-storage 0 --port-memorystore 0 --port-cloudsql-mysql 0)
  "$BIN" delete "${flags[@]}" >/dev/null 2>&1 || true

  local t0 t1 t2 t3
  t0=$(now); "$BIN" up --detach --detach-timeout 20m "${flags[@]}" >&2; t1=$(now)
  sleep 30 # let the pods settle before measuring steady state
  local node kc docker_mib pods_mib
  node="cloudburrow-$name-control-plane"
  kc="$STATE/$name/kubeconfig"
  docker_mib=$(docker stats --no-stream --format '{{.MemUsage}}' "$node" | awk '{v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v); if(u=="GiB")v*=1024; if(u=="KiB")v/=1024; printf "%.0f", v}')
  pods_mib=$(kubectl --kubeconfig "$kc" get --raw "/api/v1/nodes/$node/proxy/stats/summary" |
    jq '[.pods[].memory.workingSetBytes // 0] | add / 1048576 | floor')
  "$BIN" stop "${flags[@]}" >&2
  t2=$(now); "$BIN" up --detach --detach-timeout 20m "${flags[@]}" >&2; t3=$(now)
  "$BIN" delete "${flags[@]}" >&2

  jq -n --arg p "$profile" --arg s "$services" --argjson cold "$(elapsed "$t0" "$t1")" \
    --argjson warm "$(elapsed "$t2" "$t3")" --argjson node "$docker_mib" --argjson pods "$pods_mib" \
    '{profile:$p, services:($s|split(",")), cold_start_s:$cold, warm_start_s:$warm,
      memory_mib:{node_container:$node, pods_working_set:$pods}}'
}

results=()
for p in default all; do
  results+=("$(measure "$p" "${PROFILES[$p]}")")
done
host=$(jq -n --arg os "$(uname -sm)" --arg cpus "$(docker info --format '{{.NCPU}}')" \
  --arg mem "$(docker info --format '{{.MemTotal}}')" --arg date "$(date -u +%Y-%m-%d)" \
  --arg runner "${RUNNER_NAME:-local}" \
  '{os:$os, docker_cpus:($cpus|tonumber), docker_memory_mib:(($mem|tonumber)/1048576|floor), date:$date, runner:$runner}')
printf '%s\n' "${results[@]}" | jq -s --argjson host "$host" '{host:$host, profiles:.}' > "$OUT"
cat "$OUT"
