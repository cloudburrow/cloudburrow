#!/bin/bash
# The tool box's entrypoint (test/compat/toolbox_test.go, #2174), run as root
# once when the box starts; the tools then run in it by `docker exec` as the
# host's user, who cannot change any of this.
#
# The box is on an internal Docker network: no route out. On top of that:
#   - the filter table rejects every packet that leaves by anything but the
#     loopback, except boxd's own connections to the relay's one port;
#   - the nat table sends every DNS query, to any server, to boxd's
#     resolver, which refuses Google's names and resolves nothing outside
#     the machine;
#   - the nat table redirects every TCP connection to 127.0.0.0/8 to boxd,
#     which serves the port from the box or through the relay from the
#     host's loopback, where the instance listens.
# IPv6 is off in the box (docker run --sysctl), and ip6tables rejects it
# anyway.
set -eu
relay=$(getent ahostsv4 relay | awk 'NR==1{print $1}')
if [ -z "$relay" ]; then echo "FAIL setup: no relay"; exit 1; fi
if ip route show default | grep -q .; then echo "FAIL setup: the box has a default route"; exit 1; fi

# The host's user, so tools that ask who they are (ssh) find one.
if [ -n "${BOX_UID:-}" ] && ! getent passwd "$BOX_UID" >/dev/null; then
  echo "box:x:$BOX_UID:${BOX_GID:-$BOX_UID}:box:/home/box:/bin/bash" >> /etc/passwd
fi
if [ -n "${BOX_GID:-}" ] && ! getent group "$BOX_GID" >/dev/null; then
  echo "box:x:$BOX_GID:" >> /etc/group
fi

printf '127.0.0.1 localhost\n%s relay\n' "$relay" > /etc/hosts
echo "nameserver 127.0.0.1" > /etc/resolv.conf

boxd_uid=$(id -u boxd)
iptables -t nat -A OUTPUT -m owner --uid-owner "$boxd_uid" -j RETURN
iptables -t nat -A OUTPUT -p udp --dport 53 -j REDIRECT --to-ports 5353
iptables -t nat -A OUTPUT -p tcp --dport 53 -j REDIRECT --to-ports 5353
iptables -t nat -A OUTPUT -p tcp -d 127.0.0.0/8 -j REDIRECT --to-ports 15001
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -m owner --uid-owner "$boxd_uid" -p tcp -d "$relay" --dport 7000 -j ACCEPT
iptables -A OUTPUT -j REJECT
iptables -A INPUT -i lo -j ACCEPT
iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A INPUT -j DROP
ip6tables -A OUTPUT -o lo -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -j REJECT 2>/dev/null || ip6tables -P OUTPUT DROP 2>/dev/null || true

echo "box network set"
exec setpriv --reuid=boxd --regid=boxd --init-groups --inh-caps=-all --bounding-set=-all \
  /box/boxd serve -relay "$relay:7000"
