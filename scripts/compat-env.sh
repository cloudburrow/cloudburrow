#!/usr/bin/env bash
# compat-env.sh: the CLOUDBURROW_TEST_* environment test/compat reads, from a
# running instance (#711). It is the only place the mapping from an instance
# to those variables is written down: test/compat/README.md tells developers
# to use it, and ci.yml's compat shards and arm64.yml use it, so the two
# cannot drift. test/repo fails when a variable appears in test/compat that
# this script does not set and is not a listed exception.
#
#   cloudburrow up --detach --name ct --state-dir "$PWD/state"
#   cloudburrow wait --name ct --state-dir "$PWD/state"
#   eval "$(scripts/compat-env.sh --name ct --state-dir "$PWD/state")"
#   make test-compat
#
# Usage: scripts/compat-env.sh [options] [--] [cloudburrow flags...]
#
# The cloudburrow flags are the ones the instance was started with, the same
# ones `cloudburrow env` takes. The first argument that is not an option below
# starts them; `--` also does.
#
# Options:
#   --only A,B,...   only these variables, with or without the
#                    CLOUDBURROW_TEST_ prefix; the default is every one
#   --strict         fail, naming it, when a selected variable is empty or is
#                    a port of 0; without it, an empty variable is left out
#                    (its tests skip) and named on stderr
#   --plain          NAME=value lines, for env(1) or mapfile, instead of
#                    export statements for eval
#   --list           print every variable this script can set, and exit
#   --cli PATH       the cloudburrow binary; default bin/cloudburrow in this
#                    checkout, else cloudburrow on PATH
#   --gcloud PATH    CLOUDBURROW_TEST_GCLOUD; default gcloud on PATH
#   --tofu PATH      CLOUDBURROW_TEST_TOFU; default tofu on PATH
#   --env-json F     `cloudburrow env --format json` already written to F
#   --status-json F  `cloudburrow status --format json` already written to F
#   --runtime-json F the instance's runtime file; default up.json in its
#                    instance directory
#
# Sources, each read only when a selected variable needs it:
#   env      `cloudburrow env --format json`: the SDK endpoints, and the ADC
#            fixture, whose directory is the instance directory
#   runtime  up.json in the instance directory: the addresses the running
#            `up` bound for the services with no emulator variable
#   status   `cloudburrow status --format json`: the kind cluster's name
#   files    the instance directory's admin-token and kubeconfig, and up.log
#            for the local AI endpoint
# Requires bash and jq.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^# Requires/p' "$0" | sed 's/^# \{0,1\}//' >&2; }

# Every variable this script sets, and where each comes from. The order is
# the order of the output.
ALL=(
  CREDENTIALS KUBECONFIG CLUSTER CONTROL METADATA CONSOLE ADMIN_TOKEN CLI CLI_ARGS
  STORAGE CORS_ORIGIN PUBSUB TASKS SECRETS SCHEDULER RUN KMS LOGGING RESOURCEMANAGER
  RUN_STORAGE RUN_PUBSUB RUN_KMS RUN_SCHEDULER RUN_LOGGING
  SPANNER DATASTORE FIRESTORE BIGTABLE MEMORYSTORE MYSQL MYSQL_PASSWORD CLOUDSQL
  BIGQUERY BIGQUERY_STORAGE BIGQUERY_PROJECT
  LOCALAI GCLOUD TOFU
)

only="" strict="" plain="" cli="" gcloud="" tofu=""
env_file="" status_file="" runtime_file=""
set_gcloud="" set_tofu=""
while [ $# -gt 0 ]; do
  case "$1" in
    --only) only=${2:?--only needs a list}; shift 2 ;;
    --only=*) only=${1#*=}; shift ;;
    --strict) strict=1; shift ;;
    --plain) plain=1; shift ;;
    --list) printf 'CLOUDBURROW_TEST_%s\n' "${ALL[@]}"; exit 0 ;;
    --cli) cli=${2:?--cli needs a path}; shift 2 ;;
    --gcloud) gcloud=${2?--gcloud needs a path}; set_gcloud=1; shift 2 ;;
    --tofu) tofu=${2?--tofu needs a path}; set_tofu=1; shift 2 ;;
    --env-json) env_file=${2:?--env-json needs a file}; shift 2 ;;
    --status-json) status_file=${2:?--status-json needs a file}; shift 2 ;;
    --runtime-json) runtime_file=${2:?--runtime-json needs a file}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
# The instance's flags, as `cloudburrow env` and `status` take them. Every
# expansion of an array that can be empty is guarded, for bash 3.2's set -u.
FLAGS=("$@")

command -v jq >/dev/null || { echo "compat-env: jq is required" >&2; exit 1; }

if [ -z "$cli" ]; then
  here=$(cd "$(dirname "$0")/.." && pwd)
  if [ -x "$here/bin/cloudburrow" ]; then cli="$here/bin/cloudburrow"; else cli=$(command -v cloudburrow || true); fi
fi
# CLOUDBURROW_TEST_CLI is run from the test's own directory, so absolute.
case "$cli" in
  "") ;;
  /*) ;;
  */*) cli="$(cd "$(dirname "$cli")" && pwd)/$(basename "$cli")" ;;
  *) cli=$(command -v "$cli" || echo "$cli") ;;
esac
[ -n "$set_gcloud" ] || gcloud=$(command -v gcloud || true)
[ -n "$set_tofu" ] || tofu=$(command -v tofu || true)

# The selection, prefix stripped, each checked against ALL.
if [ -n "$only" ]; then
  IFS=', ' read -r -a SELECTED <<< "$only"
else
  SELECTED=("${ALL[@]}")
fi
for i in "${!SELECTED[@]}"; do
  n=${SELECTED[$i]#CLOUDBURROW_TEST_}
  case " ${ALL[*]} " in
    *" $n "*) SELECTED[i]=$n ;;
    *) echo "compat-env: CLOUDBURROW_TEST_$n is not a variable this script sets (see --list)" >&2; exit 2 ;;
  esac
done

# The sources, loaded once, when first needed.
ENV_JSON="" STATUS_JSON="" RUNTIME_JSON="" INSTANCE_DIR="" RUNTIME_PATH=""
need_env() {
  [ -z "$ENV_JSON" ] || return 0
  if [ -n "$env_file" ]; then
    ENV_JSON=$(cat "$env_file")
  else
    [ -n "$cli" ] || { echo "compat-env: no cloudburrow binary; pass --cli" >&2; exit 1; }
    ENV_JSON=$("$cli" env ${FLAGS[@]+"${FLAGS[@]}"} --format json) ||
      { echo "compat-env: \`cloudburrow env ${FLAGS[*]+${FLAGS[*]}} --format json\` failed; is the instance running?" >&2; exit 1; }
  fi
  # The ADC fixture is written to the instance directory (metadata.WriteADC),
  # wherever --state-dir, CLOUDBURROW_STATE_DIR or a config file put it.
  local adc
  adc=$(jq -r '.GOOGLE_APPLICATION_CREDENTIALS // empty' <<< "$ENV_JSON")
  [ -z "$adc" ] || INSTANCE_DIR=$(dirname "$adc")
}
need_runtime() {
  [ -z "$RUNTIME_PATH" ] || return 0
  if [ -n "$runtime_file" ]; then
    RUNTIME_PATH=$runtime_file
  else
    need_env
    RUNTIME_PATH=${INSTANCE_DIR:-.}/up.json
  fi
  RUNTIME_JSON=$(cat "$RUNTIME_PATH" 2>/dev/null || true)
  [ -n "$RUNTIME_JSON" ] || RUNTIME_JSON='{}'
}
need_status() {
  [ -z "$STATUS_JSON" ] || return 0
  if [ -n "$status_file" ]; then
    STATUS_JSON=$(cat "$status_file")
  else
    # 0 ready, 3 not running, 4 not ready: each still prints the report.
    local rc=0
    STATUS_JSON=$("$cli" status ${FLAGS[@]+"${FLAGS[@]}"} --format json) || rc=$?
    case "$rc" in 0|3|4) ;; *) echo "compat-env: \`cloudburrow status --format json\` exited $rc" >&2; exit 1 ;; esac
  fi
}
from_env() { need_env; VALUE=$(jq -r "$1" <<< "$ENV_JSON"); }
from_runtime() { need_runtime; VALUE=$(jq -r "$1" <<< "$RUNTIME_JSON"); }
# The instance's kubeconfig: --kubeconfig among the flags, else the one `up`
# writes to the instance directory. Never the developer's default.
kubeconfig_path() {
  local i
  for ((i = 0; i < ${#FLAGS[@]}; i++)); do
    case "${FLAGS[$i]}" in
      --kubeconfig|-kubeconfig) VALUE=${FLAGS[$((i + 1))]:-}; return ;;
      --kubeconfig=*|-kubeconfig=*) VALUE=${FLAGS[$i]#*=}; return ;;
    esac
  done
  need_env
  VALUE=${INSTANCE_DIR:+$INSTANCE_DIR/kubeconfig}
}

# The first origin the instance was started with --cors-allow-origin for
# (#677), from the flags; empty when it was given none.
cors_origin() {
  local i v=""
  for ((i = 0; i < ${#FLAGS[@]}; i++)); do
    case "${FLAGS[$i]}" in
      --cors-allow-origin|-cors-allow-origin) v=${FLAGS[$((i + 1))]:-} ;;
      --cors-allow-origin=*|-cors-allow-origin=*) v=${FLAGS[$i]#*=} ;;
      *) continue ;;
    esac
    break
  done
  VALUE=${v%%,*}
}

# The tests run the CLI with CLOUDBURROW_TEST_CLI_ARGS from test/compat, not
# from here, so a relative state directory would name another one.
warn_relative_state_dir() {
  local i d
  for ((i = 0; i < ${#FLAGS[@]}; i++)); do
    case "${FLAGS[$i]}" in
      --state-dir|-state-dir) d=${FLAGS[$((i + 1))]:-} ;;
      --state-dir=*|-state-dir=*) d=${FLAGS[$i]#*=} ;;
      *) continue ;;
    esac
    case "$d" in
      /*) ;;
      *) echo "compat-env: --state-dir $d is relative, but the tests run the CLI from test/compat; pass an absolute path" >&2 ;;
    esac
  done
}

# value <name>: sets VALUE to the variable's value, empty when the instance
# has none.
value() {
  VALUE=""
  case "$1" in
    # The generated ADC fixture, for the official auth library test (#346).
    # The harness refuses GOOGLE_APPLICATION_CREDENTIALS itself; the test sets
    # it only for its own call.
    CREDENTIALS) from_env '.GOOGLE_APPLICATION_CREDENTIALS // empty' ;;
    # The instance's own kubeconfig and kind cluster, for the tests that
    # restart a backend, run a pod or load a fixture image (#346).
    KUBECONFIG) kubeconfig_path; [ -s "$VALUE" ] || VALUE="" ;;
    CLUSTER) need_status; VALUE=$(jq -r '.cluster.name // empty' <<< "$STATUS_JSON") ;;
    # The admin API, the metadata server (which also serves IAM Credentials,
    # #303) and the console are not SDK endpoints: the runtime file records
    # the addresses they bound.
    CONTROL) from_runtime '.endpoints.control // empty' ;;
    METADATA) from_runtime '.endpoints.metadata // empty' ;;
    CONSOLE) from_runtime '.endpoints.console // empty' ;;
    # The admin token every /admin call needs (#553), which `up` keeps
    # owner-only in the instance directory.
    ADMIN_TOKEN) need_env; [ -z "$INSTANCE_DIR" ] || [ ! -r "$INSTANCE_DIR/admin-token" ] || VALUE=$(cat "$INSTANCE_DIR/admin-token") ;;
    # The CLI and the flags naming this instance, for tests that drive a
    # cloudburrow command, such as `cloudburrow terraform`.
    CLI) VALUE=$cli ;;
    CLI_ARGS) VALUE="${FLAGS[*]+${FLAGS[*]}}"; warn_relative_state_dir ;;
    STORAGE|RUN_STORAGE) from_env '.STORAGE_EMULATOR_HOST // empty' ;;
    # An origin the storage server answers beyond loopback ones (#677).
    CORS_ORIGIN) cors_origin ;;
    PUBSUB|RUN_PUBSUB) from_env '.PUBSUB_EMULATOR_HOST // empty' ;;
    # Cloud Tasks, Secret Manager and Cloud Run have no emulator variable.
    TASKS) from_runtime '.endpoints.tasks // empty' ;;
    SECRETS) from_runtime '.endpoints.secretmanager // empty' ;;
    RUN) from_runtime '.endpoints.run // empty' ;;
    SCHEDULER|RUN_SCHEDULER) from_env '.CLOUDBURROW_SCHEDULER_ENDPOINT // empty' ;;
    KMS|RUN_KMS) from_env '.CLOUDBURROW_KMS_ENDPOINT // empty' ;;
    LOGGING|RUN_LOGGING) from_env '.CLOUDBURROW_LOGGING_ENDPOINT // empty' ;;
    RESOURCEMANAGER) from_env '.CLOUDBURROW_RESOURCEMANAGER_ENDPOINT // empty' ;;
    SPANNER) from_env '.SPANNER_EMULATOR_HOST // empty' ;;
    DATASTORE) from_env '.DATASTORE_EMULATOR_HOST // empty' ;;
    FIRESTORE) from_env '.FIRESTORE_EMULATOR_HOST // empty' ;;
    BIGTABLE) from_env '.BIGTABLE_EMULATOR_HOST // empty' ;;
    # Memorystore, Cloud SQL for MySQL and Cloud SQL for PostgreSQL from the
    # host and port variables env exports for each (#296, #297, #584).
    MEMORYSTORE) from_env 'if .REDIS_HOST and .REDIS_PORT then "\(.REDIS_HOST):\(.REDIS_PORT)" else empty end' ;;
    MYSQL) from_env 'if .MYSQL_HOST and .MYSQL_PORT then "\(.MYSQL_HOST):\(.MYSQL_PORT)" else empty end' ;;
    MYSQL_PASSWORD) from_env '.MYSQL_PASSWORD // empty' ;;
    CLOUDSQL) from_env 'if .PGHOST and .PGPORT then "\(.PGHOST):\(.PGPORT)" else empty end' ;;
    # BigQuery has no emulator variable in any client library; the project
    # is the instance's, the one project the emulator serves (#277).
    BIGQUERY) from_env '.CLOUDBURROW_BIGQUERY_ENDPOINT // empty' ;;
    BIGQUERY_STORAGE) from_env '.CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT // empty' ;;
    BIGQUERY_PROJECT) from_env '.GOOGLE_CLOUD_PROJECT // empty' ;;
    # The local generation endpoint, only with --local-ai-model: neither env
    # nor the runtime file carries it, so the line `up` prints in its log.
    LOCALAI)
      need_runtime
      local log
      log=$(jq -r '.log // empty' <<< "$RUNTIME_JSON")
      [ -n "$log" ] || log=${INSTANCE_DIR:+$INSTANCE_DIR/up.log}
      [ -z "$log" ] || [ ! -r "$log" ] || VALUE=$(sed -n 's|^local AI: *http://||p' "$log" | tail -n 1) ;;
    GCLOUD) VALUE=$gcloud ;;
    TOFU) VALUE=$tofu ;;
    *) echo "compat-env: internal error: no source for $1" >&2; exit 2 ;;
  esac
}

# In strict mode, what the variables came from, for the failure's log.
dump_sources() {
  [ -z "$ENV_JSON" ] || printf '%s\n' "$ENV_JSON" >&2
  [ -z "$RUNTIME_PATH" ] || cat "$RUNTIME_PATH" >&2 2>/dev/null || true
}

OUT=() UNSET=()
for n in "${SELECTED[@]}"; do
  value "$n"
  v=CLOUDBURROW_TEST_$n
  if [ -n "$strict" ]; then
    # A port of 0 would mean env fell back to configuration.
    [ -n "$VALUE" ] || { echo "$v is empty" >&2; dump_sources; exit 1; }
    case "$VALUE" in *:0) echo "env exported an unbound port: $v=$VALUE" >&2; exit 1 ;; esac
  elif [ -z "$VALUE" ]; then
    UNSET+=("$v")
    continue
  fi
  if [ -n "$plain" ]; then
    OUT+=("$v=$VALUE")
  else
    q=\'
    OUT+=("export $v='${VALUE//$q/$q\\$q$q}'")
  fi
done
[ ${#OUT[@]} -eq 0 ] || printf '%s\n' "${OUT[@]}"
if [ ${#UNSET[@]} -gt 0 ]; then
  echo "compat-env: not set, so the tests that need them skip: ${UNSET[*]}" >&2
fi
