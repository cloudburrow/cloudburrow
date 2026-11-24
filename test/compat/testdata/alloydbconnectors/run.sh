#!/bin/bash
# The entrypoint of TestAlloyDBConnectorsSendRealSQL's no-egress container
# (test/compat/alloydbconnectors_test.go), as testdata/egress/run.sh: an
# internal network, a default route onto it so the nat table sees every
# attempt, and the sink, which logs each one. EGRESS_FORWARDS lists the
# sink's forwarders, listen=relay-port: the instance's admin API (plaintext
# and TLS) and the connectors' listener at the instance's address, port
# 5433, which is where the connectors dial.
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
for f in $EGRESS_FORWARDS; do
  listen=${f%=*}; port=${f##*=}
  args+=(-forward "$listen=$relay:$port")
done
/egress/sink "${args[@]}" &
sleep 1
# The JVM trusts the AlloyDB CA for the admin API's TLS endpoint; gRPC's C
# core (Python) reads it from GRPC_DEFAULT_SSL_ROOTS_FILE_PATH.
keytool -importcert -noprompt -alias cloudburrow -file /egress/ca.pem -keystore /tmp/trust.jks -storepass changeit >/dev/null 2>&1
export GRPC_DEFAULT_SSL_ROOTS_FILE_PATH=/egress/ca.pem
for client in "$@"; do
  case "$client" in
    self) timeout 3 bash -c 'exec 3<>/dev/tcp/192.0.2.1/443' 2>/dev/null; echo "OK self" ;;
    go) timeout 120 /egress/goprobe || echo "FAIL go: exit $?" ;;
    python) timeout 120 /py/bin/python -I /egress/probe_connector.py || echo "FAIL python: exit $?" ;;
    java) timeout 180 java -Djavax.net.ssl.trustStore=/tmp/trust.jks -Djavax.net.ssl.trustStorePassword=changeit \
            -cp '/java/lib/*' /egress/Probe.java || echo "FAIL java: exit $?" ;;
    proxy)
      alloydb-auth-proxy "$ADB_INSTANCE" --alloydbadmin-api-endpoint "http://$ADB_API" --token "$ADB_TOKEN" \
        --port 5000 --address 127.0.0.1 --disable-built-in-telemetry > /tmp/proxy.log 2>&1 &
      for _ in $(seq 1 60); do grep -q "ready for new connections" /tmp/proxy.log && break; sleep 0.5; done
      timeout 120 /py/bin/python -I /egress/probe_proxy.py || echo "FAIL proxy: exit $?"
      kill %2 2>/dev/null; sleep 1
      sed 's/^/proxy: /' /tmp/proxy.log ;;
  esac
done
sleep 1
echo "DONE"
