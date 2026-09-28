#!/usr/bin/env bash
# bigquery-engine-soak.sh: reproduce #989 on an instance of your own.
#
# The pinned BigQuery emulator's SQL engine (goccy/go-googlesql v0.3.0, a
# WebAssembly module translated to Go) addresses its memory with signed
# 32-bit offsets, so once that memory passes 2 GiB a call into it panics
# "slice bounds out of range [-2147...:]": in a request, which the emulator
# answers 500 "googlesqlite: panic ..."; or in a finalizer, which kills the
# process (exit code 2). The memory grows with every query job that returns
# rows (the emulator keeps each result in a dataset of its own, and the
# engine builds a catalog for each) and with every DROP TABLE (which
# rebuilds the catalogs). Since #1017 the front has query results written
# to one dataset (internal/bigqueryfront/results.go), which keeps the
# memory from growing per job. See docs/compatibility.md, BigQuery.
#
# This runs query jobs, and a CREATE TABLE and DROP TABLE every --ddl-every
# jobs, against the instance's BigQuery endpoint, and prints, every
# --report-every jobs, the emulator container's memory and restart count.
# It stops at the first answer that is not 200, at a restart, or after
# --jobs jobs. It changes nothing but a dataset of its own, soak_989.
#
#   scripts/bigquery-engine-soak.sh --name <instance> [--jobs 400]
#
# Requires curl, jq and kubectl; reads the instance's kubeconfig and
# endpoints through `cloudburrow env` and `cloudburrow status`.
set -euo pipefail

name="" jobs=400 ddl_every=5 report_every=10 cli=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) name=${2:?}; shift 2 ;;
    --jobs) jobs=${2:?}; shift 2 ;;
    --ddl-every) ddl_every=${2:?}; shift 2 ;;
    --report-every) report_every=${2:?}; shift 2 ;;
    --cli) cli=${2:?}; shift 2 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$name" ] || { echo "--name is required: an instance of your own" >&2; exit 2; }
if [ -z "$cli" ]; then
  root=$(cd "$(dirname "$0")/.." && pwd)
  if [ -x "$root/bin/cloudburrow" ]; then cli=$root/bin/cloudburrow; else cli=cloudburrow; fi
fi

envjson=$("$cli" env --name "$name" --format json)
endpoint=$(jq -r '.CLOUDBURROW_BIGQUERY_ENDPOINT // empty' <<<"$envjson")
project=$(jq -r '.GOOGLE_CLOUD_PROJECT // empty' <<<"$envjson")
creds=$(jq -r '.GOOGLE_APPLICATION_CREDENTIALS // empty' <<<"$envjson")
if [ -z "$endpoint" ] || [ -z "$project" ]; then
  echo "no BigQuery endpoint for $name (started with --services bigquery?)" >&2
  exit 1
fi
export KUBECONFIG=${creds%/*}/kubeconfig
ns=$(kubectl get pods -A -l app=bigquery,cloudburrow.dev/owned=true -o jsonpath='{.items[0].metadata.namespace}')
pod=pod/$(kubectl get pods -n "$ns" -l app=bigquery,cloudburrow.dev/owned=true -o jsonpath='{.items[0].metadata.name}')
node=$(kubectl get -n "$ns" "$pod" -o jsonpath='{.spec.nodeName}')

restarts() {
  kubectl get -n "$ns" "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="bigquery")].restartCount}'
}
memory() {
  # The emulator container's memory, as the node's container runtime
  # reports it (kind nodes are containers of the host's Docker).
  local id
  id=$(kubectl get -n "$ns" "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="bigquery")].containerID}')
  docker exec "$node" crictl stats -o json "${id#containerd://}" 2>/dev/null |
    jq -r '.stats[0].memory.workingSetBytes.value // empty | tonumber / 1048576 | floor | tostring + " MiB"' || echo "?"
}
post() { # path body -> prints status, body in $out
  out=$(curl -sS -o - -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data "$2" "$endpoint/bigquery/v2/projects/$project$1")
  code=${out##*$'\n'}
  out=${out%$'\n'*}
}

start=$(restarts)
echo "instance $name: $endpoint, project $project, pod $pod, restarts $start, memory $(memory)"
post /datasets '{"datasetReference":{"datasetId":"soak_989"}}'
[ "$code" = 200 ] || [ "$code" = 409 ] || { echo "datasets.insert: $code $out" >&2; exit 1; }

for i in $(seq 1 "$jobs"); do
  post /jobs "{\"configuration\":{\"query\":{\"query\":\"SELECT $i AS n, 'row' AS s\",\"useLegacySql\":false}}}"
  if [ "$code" != 200 ]; then
    echo "job $i: HTTP $code: $(jq -c '.error.message // .' <<<"$out" 2>/dev/null | cut -c1-300)"
    break
  fi
  if [ $((i % ddl_every)) = 0 ]; then
    for sql in "CREATE TABLE soak_989.t$i (a INT64)" "DROP TABLE soak_989.t$i"; do
      post /queries "{\"query\":\"$sql\",\"useLegacySql\":false}"
      if [ "$code" != 200 ]; then
        echo "$sql, after job $i: HTTP $code: $(jq -c '.error.message // .' <<<"$out" 2>/dev/null | cut -c1-300)"
        break 2
      fi
    done
  fi
  if [ $((i % report_every)) = 0 ]; then
    echo "job $i: memory $(memory), restarts $(restarts)"
  fi
  if [ "$(restarts)" != "$start" ]; then
    echo "job $i: the emulator restarted (restart count $(restarts), was $start)"
    break
  fi
done
echo "done: restarts $(restarts) (was $start), memory $(memory)"
