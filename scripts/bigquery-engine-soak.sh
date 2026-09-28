#!/usr/bin/env bash
# bigquery-engine-soak.sh: measure the BigQuery emulator's SQL engine
# growing, on an instance of your own (#989, #1017, #1057, #1059).
#
# The pinned BigQuery emulator's SQL engine (goccy/go-googlesql v0.3.0, a
# WebAssembly module translated to Go) addresses its memory with signed
# 32-bit offsets, so once that memory passes 2 GiB a call into it panics
# "slice bounds out of range [-2147...:]": in a request, which the emulator
# answers 500 "googlesqlite: panic ..."; or in a finalizer, which kills the
# process (exit code 2). Its engine (googlesqlite v0.3.1) builds a catalog
# of every builtin function for each dataset that holds a table, and
# builds all of them again at each DROP TABLE, DROP VIEW, DROP FUNCTION and
# at the end of a script that made a TEMP table (resetCatalog). Since
# #1017 the front has query results written to one dataset
# (internal/bigqueryfront/results.go), so a query job no longer adds one.
# See docs/compatibility.md, BigQuery.
#
# --phase picks what is run, --jobs times (each an "operation"):
#   query     a query job, and a CREATE TABLE and DROP TABLE every
#             --ddl-every jobs (the default; #989, #1017)
#   ddl       a CREATE TABLE and a DROP TABLE (jobs.query)
#   dataset   datasets.insert, a CREATE TABLE in it, datasets.delete with
#             deleteContents
#   load      a CSV load job (a multipart upload) with a schema into a new
#             table, then tables.delete of it
#   float     a CSV load job with autodetect into a new table of a FLOAT
#             column, which the front makes again as FLOAT (#1000), then
#             tables.delete of it
#   script    a query job of a script: DECLARE, CREATE TEMP TABLE, SELECT
#   failed    a script given to jobs.query that makes a table and fails,
#             which the emulator rolls back and the front puts back in
#             step (#955), answering 501
#   function  CREATE FUNCTION and DROP FUNCTION (jobs.query)
#   replace   CREATE OR REPLACE FUNCTION of one function that exists
#   copy      a copy job with WRITE_TRUNCATE of one table onto another
#   merge     a MERGE whose source is a subquery (jobs.query)
#   orreplace CREATE OR REPLACE TABLE ... AS SELECT of a table that exists
# --datasets N first makes N datasets of one table each (soak_989_bg_*),
# which every catalog rebuild then builds a catalog for: the cost of a
# rebuild grows with them. It prints, every --report-every operations, the
# emulator container's memory, restart count and the time so far, and a
# last line with the time per operation. It stops at the first answer that
# is not 200, at a restart, or after --jobs operations. It changes nothing
# but datasets of its own, soak_989*; --cleanup deletes them first.
#
#   scripts/bigquery-engine-soak.sh --name <instance> [--phase query] [--jobs 400]
#
# Requires curl, jq and kubectl; reads the instance's kubeconfig and
# endpoints through `cloudburrow env` and `cloudburrow status`.
set -euo pipefail

name="" jobs=400 ddl_every=5 report_every=10 cli="" phase=query datasets=0 cleanup=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) name=${2:?}; shift 2 ;;
    --jobs) jobs=${2:?}; shift 2 ;;
    --ddl-every) ddl_every=${2:?}; shift 2 ;;
    --report-every) report_every=${2:?}; shift 2 ;;
    --cli) cli=${2:?}; shift 2 ;;
    --phase) phase=${2:?}; shift 2 ;;
    --datasets) datasets=${2:?}; shift 2 ;;
    --cleanup) cleanup=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$name" ] || { echo "--name is required: an instance of your own" >&2; exit 2; }
case "$phase" in
  query|ddl|dataset|load|float|script|failed|function|replace|copy|merge|orreplace) ;;
  *) echo "unknown --phase $phase" >&2; exit 2 ;;
esac
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
call() { # method path -> status in $code, body in $out
  out=$(curl -sS -o - -w '\n%{http_code}' -X "$1" "$endpoint/bigquery/v2/projects/$project$2")
  code=${out##*$'\n'}
  out=${out%$'\n'*}
}
sql() { # jobs.query of $1
  post /queries "$(jq -nc --arg q "$1" '{query: $q, useLegacySql: false}')"
}
upload() { # config-json csv -> a multipart load job
  local b=soak$RANDOM$RANDOM
  out=$(printf -- '--%s\r\nContent-Type: application/json\r\n\r\n%s\r\n--%s\r\nContent-Type: text/csv\r\n\r\n%s\r\n--%s--\r\n' \
      "$b" "$1" "$b" "$2" "$b" |
    curl -sS -o - -w '\n%{http_code}' -X POST -H "Content-Type: multipart/related; boundary=$b" \
      --data-binary @- "$endpoint/upload/bigquery/v2/projects/$project/jobs?uploadType=multipart")
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  if [ "$code" = 200 ] && [ "$(jq -r '.status.errorResult.message // empty' <<<"$out")" != "" ]; then code=job-failed; fi
}
fail() { # what
  echo "$1: HTTP $code: $(jq -c '.error.message // .status.errorResult.message // .' <<<"$out" 2>/dev/null | cut -c1-300)"
}
now() { date +%s; }
ok() { [ "$code" = 200 ]; }

# op i: one operation of the phase; returns non-zero, having said why, at
# a failure.
op() {
  local i=$1 ds=soak_989
  case "$phase" in
    query)
      post /jobs "{\"configuration\":{\"query\":{\"query\":\"SELECT $i AS n, 'row' AS s\",\"useLegacySql\":false}}}"
      ok || { fail "job $i"; return 1; }
      if [ $((i % ddl_every)) = 0 ]; then
        for q in "CREATE TABLE $ds.t$i (a INT64)" "DROP TABLE $ds.t$i"; do
          sql "$q"; ok || { fail "$q, after job $i"; return 1; }
        done
      fi ;;
    ddl)
      for q in "CREATE TABLE $ds.t$i (a INT64)" "DROP TABLE $ds.t$i"; do
        sql "$q"; ok || { fail "$q"; return 1; }
      done ;;
    dataset)
      post /datasets "{\"datasetReference\":{\"datasetId\":\"soak_989_d$i\"}}"; ok || { fail "datasets.insert $i"; return 1; }
      sql "CREATE TABLE soak_989_d$i.t (a INT64)"; ok || { fail "CREATE TABLE in soak_989_d$i"; return 1; }
      call DELETE "/datasets/soak_989_d$i?deleteContents=true"; [ "$code" = 204 ] || ok || { fail "datasets.delete $i"; return 1; } ;;
    load)
      upload "{\"configuration\":{\"load\":{\"destinationTable\":{\"projectId\":\"$project\",\"datasetId\":\"$ds\",\"tableId\":\"l$i\"},\"sourceFormat\":\"CSV\",\"schema\":{\"fields\":[{\"name\":\"a\",\"type\":\"INTEGER\"},{\"name\":\"s\",\"type\":\"STRING\"}]}}}}" \
        "$i,x"$'\n'"$((i + 1)),y"
      ok || { fail "load $i"; return 1; }
      call DELETE "/datasets/$ds/tables/l$i"; [ "$code" = 204 ] || ok || { fail "tables.delete l$i"; return 1; } ;;
    float)
      upload "{\"configuration\":{\"load\":{\"destinationTable\":{\"projectId\":\"$project\",\"datasetId\":\"$ds\",\"tableId\":\"f$i\"},\"sourceFormat\":\"CSV\",\"autodetect\":true}}}" \
        "a,x"$'\n'"1.5,$i"$'\n'"2.5,y"
      ok || { fail "load $i"; return 1; }
      call DELETE "/datasets/$ds/tables/f$i"; [ "$code" = 204 ] || ok || { fail "tables.delete f$i"; return 1; } ;;
    script)
      post /jobs "$(jq -nc --arg q "DECLARE x INT64 DEFAULT $i; CREATE TEMP TABLE tt AS SELECT x AS a; SELECT a FROM tt" \
        '{configuration: {query: {query: $q, useLegacySql: false}}}')"
      ok && [ "$(jq -r '.status.errorResult.message // empty' <<<"$out")" = "" ] || { fail "script $i"; return 1; } ;;
    failed)
      sql "CREATE TABLE $ds.c$i AS SELECT 1 AS a; SELECT * FROM nope.nope"
      # 501: the front's answer, having put the catalog back in step.
      case "$code" in 400|404|501) ;; *) fail "failed script $i (want 400, 404 or 501)"; return 1 ;; esac ;;
    function)
      for q in "CREATE FUNCTION $ds.f$i(x INT64) AS (x + $i)" "DROP FUNCTION $ds.f$i"; do
        sql "$q"; ok || { fail "$q"; return 1; }
      done ;;
    replace)
      sql "CREATE OR REPLACE FUNCTION $ds.fr(x INT64) AS (x + $i)"; ok || { fail "replace $i"; return 1; } ;;
    copy)
      post /jobs "{\"configuration\":{\"copy\":{\"sourceTable\":{\"projectId\":\"$project\",\"datasetId\":\"$ds\",\"tableId\":\"src\"},\"destinationTable\":{\"projectId\":\"$project\",\"datasetId\":\"$ds\",\"tableId\":\"cp\"},\"writeDisposition\":\"WRITE_TRUNCATE\"}}}"
      ok && [ "$(jq -r '.status.errorResult.message // empty' <<<"$out")" = "" ] || { fail "copy $i"; return 1; } ;;
    merge)
      sql "MERGE $ds.src T USING (SELECT 1 AS a, 'm$i' AS s) S ON T.a = S.a WHEN MATCHED THEN UPDATE SET s = S.s"
      ok || { fail "merge $i"; return 1; } ;;
    orreplace)
      sql "CREATE OR REPLACE TABLE $ds.r AS SELECT $i AS a"; ok || { fail "replace $i"; return 1; } ;;
  esac
}

start=$(restarts)
echo "instance $name: $endpoint, project $project, pod $pod, restarts $start, memory $(memory), phase $phase"
if [ -n "$cleanup" ]; then
  call GET "/datasets?all=true&maxResults=1000"
  for d in $(jq -r '.datasets[]?.datasetReference.datasetId | select(startswith("soak_989"))' <<<"$out"); do
    call DELETE "/datasets/$d?deleteContents=true"
  done
  echo "cleaned up: memory $(memory)"
fi
post /datasets '{"datasetReference":{"datasetId":"soak_989"}}'
[ "$code" = 200 ] || [ "$code" = 409 ] || { echo "datasets.insert: $code $out" >&2; exit 1; }
for d in $(seq 1 "$datasets"); do
  post /datasets "{\"datasetReference\":{\"datasetId\":\"soak_989_bg_$d\"}}"
  sql "CREATE TABLE IF NOT EXISTS soak_989_bg_$d.t (a INT64)"
  ok || { fail "background dataset $d"; exit 1; }
done
case "$phase" in
  copy|merge) sql "CREATE TABLE IF NOT EXISTS soak_989.src AS SELECT 1 AS a, 'x' AS s"
    sql "CREATE TABLE IF NOT EXISTS soak_989.cp AS SELECT 1 AS a, 'x' AS s" ;;
  orreplace) sql "CREATE TABLE IF NOT EXISTS soak_989.r AS SELECT 0 AS a" ;;
  replace) sql "CREATE OR REPLACE FUNCTION soak_989.fr(x INT64) AS (x)" ;;
esac
[ "$datasets" = 0 ] || echo "$datasets background datasets: memory $(memory)"

t0=$(now)
done_ops=0
for i in $(seq 1 "$jobs"); do
  op "$i" || break
  done_ops=$i
  if [ $((i % report_every)) = 0 ]; then
    echo "op $i: memory $(memory), restarts $(restarts), $(($(now) - t0)) s"
  fi
  if [ "$(restarts)" != "$start" ]; then
    echo "op $i: the emulator restarted (restart count $(restarts), was $start)"
    break
  fi
done
t=$(($(now) - t0))
per=$(awk -v t="$t" -v n="$done_ops" 'BEGIN { if (n > 0) printf "%.2f", t / n; else print "-" }')
echo "done: $phase, $done_ops operations in $t s ($per s each), restarts $(restarts) (was $start), memory $(memory)"
