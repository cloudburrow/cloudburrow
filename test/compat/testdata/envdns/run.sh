#!/bin/bash
# The entrypoint of TestEnvResolvesNoGoogleName's container
# (test/compat/envdns_test.go). The network setup is testdata/egress's
# run.sh: the container is on an internal network, with no route out; the
# nat table sends every DNS query to the sink's resolver, which logs the
# name ("EGRESS dns NAME type N"), and every connection off the machine to
# the sink, which logs and closes it. Only the sink's forwarders reach the
# relay, which forwards the instance's ports: EGRESS_PORTS, served on the
# container's loopback where `cloudburrow env` names them.
set -u
relay=$(getent hosts relay | awk '{print $1}')
if [ -z "$relay" ]; then echo "FAIL setup: no relay"; exit 1; fi
ip route add default dev eth0 || { echo "FAIL setup: route"; exit 1; }
iptables -t nat -A OUTPUT -d 127.0.0.0/8 -j RETURN
iptables -t nat -A OUTPUT -d "$relay" -p tcp -j RETURN
iptables -t nat -A OUTPUT -p tcp -j REDIRECT --to-ports 9999 || { echo "FAIL setup: iptables"; exit 1; }
iptables -t nat -A OUTPUT -p udp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A OUTPUT -p udp -j REDIRECT --to-ports 9998
ip6tables -P OUTPUT DROP 2>/dev/null
echo "nameserver 127.0.0.1" > /etc/resolv.conf
args=()
for p in $EGRESS_PORTS; do args+=(-forward "127.0.0.1:$p=$relay:$p"); done
/egress/sink "${args[@]}" &
sleep 1
gcloud=/opt/google-cloud-sdk/bin/gcloud
# gcloud prints OK only when the command succeeded; its output follows.
g() {
  local name=$1; shift
  if out=$("$@" 2>&1); then echo "OK gcloud $name"; else echo "RAN gcloud $name (failed)"; fi
  printf '%s\n' "$out" | sed 's/^/  | /' | tail -n 15
}
for client in "$@"; do
  case "$client" in
    # The control: a lookup of a Google name must be logged, or the log is
    # broken and an empty one proves nothing.
    self) getent hosts control.googleapis.com >/dev/null; echo "OK self" ;;
    # Under `cloudburrow env` alone: a fresh configuration, nothing else.
    gcloud-env)
      export CLOUDSDK_CONFIG=$(mktemp -d)
      g version "$gcloud" version
      g config-list "$gcloud" config list
      g auth-list "$gcloud" auth list
      g storage-ls-no-account "$gcloud" storage ls
      CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true g storage-ls "$gcloud" storage ls
      CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true g pubsub-topics "$gcloud" pubsub topics list
      ;;
    # The second control: the variables before #2038, which must show the
    # lookup of metadata.google.internal this test exists to catch.
    gcloud-before)
      unset GCE_METADATA_ROOT GCE_METADATA_IP CLOUDSDK_METRICS_ENVIRONMENT CLOUDSDK_CORE_CHECK_GCE_METADATA
      export CLOUDSDK_CONFIG=$(mktemp -d)
      CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true g pubsub-topics "$gcloud" pubsub topics list
      ;;
    # The fixture as gcloud's credential (auth/credential_file_override):
    # with credentials on, gcloud's own google-auth would start the
    # Regional Access Boundary lookup (#1981); `env` exports
    # CLOUDSDK_AUTH_DISABLE_CREDENTIALS, so none is loaded (#2040).
    gcloud-fixture)
      export CLOUDSDK_CONFIG=$(mktemp -d) CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=$GOOGLE_APPLICATION_CREDENTIALS
      g storage-ls "$gcloud" storage ls
      ;;
    # Under gcloud-setup's configuration alone (EGRESS_GCLOUD_CONFIG, a
    # copy written by the test), with no `cloudburrow env` variable.
    gcloud-setup)
      export CLOUDSDK_CONFIG=$EGRESS_GCLOUD_CONFIG CLOUDSDK_ACTIVE_CONFIG_NAME=$EGRESS_GCLOUD_NAME
      g version "$gcloud" version
      g storage-ls "$gcloud" storage ls
      g pubsub-topics "$gcloud" pubsub topics list
      ;;
    go) /egress/goprobe ;;
    python) /py/bin/python -I /egress/probe.py ;;
    node) NODE_PATH=/node/node_modules node /egress/probe.js ;;
    node-token) NODE_PATH=/node/node_modules node /egress/probe.js token ;;
    java) java -cp '/java:/java/lib/*' Probe ;;
  esac
done
sleep 1
echo "DONE"
