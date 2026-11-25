#!/bin/bash
# The tools container's entrypoint (test/compat/toolscontainer_test.go,
# #2174). The container is on an internal Docker network, with no route out;
# the tools run in it as the test's own uid, through `docker exec`. A
# default route onto that network lets the nat table see every attempt:
#
#   - a TCP connection to a loopback address goes to the sink's carrier on
#     :9997, which takes it to the relay, and so to the same port on the
#     host's loopback, where the instance and the test's own servers listen;
#   - a DNS query, to any server, gets the sink's answer, 198.18.0.1 for any
#     name (routed nowhere), and is logged;
#   - any other TCP connection or UDP datagram, a real Google host's
#     included, is logged and refused by the sink (testdata/egress/sink);
#   - only root, the sink, may reach the relay; IPv6 is refused.
#
# Each attempt is an EGRESS line in /cblog/egress.log, which the test reads
# and reports as an escape.
set -u
relay=$(getent hosts relay | awk '{print $1}')
if [ -z "$relay" ]; then echo "FAIL setup: no relay"; exit 1; fi
ip route add default dev eth0 || { echo "FAIL setup: route"; exit 1; }
set -e
iptables -t nat -A OUTPUT -d 127.0.0.0/8 -p tcp -m multiport --dports 53,9997,9998,9999 -j RETURN
iptables -t nat -A OUTPUT -d 127.0.0.0/8 -p tcp -j REDIRECT --to-ports 9997
iptables -t nat -A OUTPUT -d 127.0.0.0/8 -j RETURN
iptables -t nat -A OUTPUT -d "$relay" -p tcp --dport 7000 -m owner --uid-owner 0 -j RETURN
iptables -t nat -A OUTPUT -p tcp -j REDIRECT --to-ports 9999
iptables -t nat -A OUTPUT -p udp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A OUTPUT -p udp -j REDIRECT --to-ports 9998
# Refused at once rather than dropped, so a client that tries ::1 first
# falls back to 127.0.0.1 without waiting out a timeout. The network has no
# IPv6 route out either way.
ip6tables -A OUTPUT -p tcp -j REJECT --reject-with tcp-reset 2>/dev/null || true
ip6tables -A OUTPUT -j REJECT 2>/dev/null || true
echo "nameserver 127.0.0.1" > /etc/resolv.conf
exec /cbtools/sink -loopback "127.0.0.1:9997=$relay:7000" -token-env CB_TOOLS_TOKEN \
  -log /cblog/egress.log -ready /cblog/ready
