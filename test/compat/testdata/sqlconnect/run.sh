#!/bin/sh
# The entrypoint of TestGcloudSQLConnectInContainer's containers
# (test/compat/sqlconnect2208_test.go). The container is on an internal
# Docker network, with no route out; the relay is the one container that
# reaches the instance. The sink serves each of the instance's ports on the
# container's loopback, where `cloudburrow env` names them, and port 3307,
# where connectSettings tells the Auth Proxy to dial, from the relay's
# port for the instance's connector listener, wherever that listens.
#
#   run.sh connect ARGS...  runs gcloud sql connect ARGS, its stdin the
#                           password and the statements;
#   run.sh proxy CONN PORT CLIENT ARGS...  runs Google's Auth Proxy for CONN
#                           on PORT, then CLIENT ARGS once it is ready.
set -u
relay=$(getent hosts relay | awk '{print $1}')
if [ -z "$relay" ]; then echo "FAIL setup: no relay"; exit 1; fi
args=""
for p in $SQLCONNECT_PORTS; do args="$args -forward 127.0.0.1:$p=$relay:$p"; done
args="$args -forward 127.0.0.1:3307=$relay:$SQLCONNECT_CONNECTOR_PORT"
# shellcheck disable=SC2086
/egress/sink $args >/tmp/sink.log 2>&1 &
sleep 1
export CLOUDSDK_CONFIG=/tmp/gcloud
mode=$1
shift
case "$mode" in
  connect)
    exec gcloud sql connect "$@" ;;
  proxy)
    conn=$1 port=$2
    shift 2
    cloud-sql-proxy --sqladmin-api-endpoint "$CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL" --token cloudburrow \
      --auto-iam-authn=false --address 127.0.0.1 --port "$port" "$conn" >/tmp/proxy.log 2>&1 &
    i=0
    until grep -q "ready for new connections" /tmp/proxy.log; do
      i=$((i + 1))
      if [ "$i" -gt 60 ]; then echo "FAIL proxy not ready"; cat /tmp/proxy.log; exit 1; fi
      sleep 1
    done
    echo "PROXY $(grep -o 'Listening on [^ ]*' /tmp/proxy.log | head -1)"
    "$@"
    code=$?
    echo "EXIT $code"
    exit "$code" ;;
esac
echo "FAIL unknown mode $mode"
exit 2
