#!/bin/bash
# The no-egress container's entrypoint (test/compat/egresscontainer_test.go).
# The container is on an internal network, with no route out. A default
# route onto that network lets the nat table see every attempt, and
# redirects it to the sink (testdata/egress/sink), which logs it. Only the
# sink's forwarders reach the relay, the one container that can reach the
# instance's ports.
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
for client in "$@"; do
  case "$client" in
    # The control: a connection to a documentation address must be counted,
    # or the counter is broken and a zero proves nothing.
    self) timeout 3 bash -c 'exec 3<>/dev/tcp/192.0.2.1/443' 2>/dev/null; echo "OK self" ;;
    go) /egress/goprobe ;;
    python) /py/bin/python -I /egress/probe.py ;;
    node) NODE_PATH=/node/node_modules node /egress/probe.js ;;
  esac
done
sleep 1
echo "DONE"
