# Puts private services addresses on a Dataflow job's workers' loopback
# (internal/beamrun): each IP given is added, as a /32, to the "lo" of the
# network namespace this runs in (the job server's, which the workers
# share), so that a worker's connection to it, a Cloud SQL instance's
# private IP on the job's VPC network, is answered there by the job's
# private services relay (relay.py, listening on IP:PORT), which sends it
# on to the job's relay and from there to the instance's server, and
# nothing else. It runs once, with CAP_NET_ADMIN, and exits; nothing it
# adds is reachable from outside the namespace.
#
# It speaks rtnetlink (RTM_NEWADDR) itself: the SDK images it runs in have
# no iproute2.
import socket
import struct
import sys

RTM_NEWADDR = 20
NLMSG_ERROR = 2
NLM_F_REQUEST, NLM_F_ACK, NLM_F_EXCL, NLM_F_CREATE = 0x1, 0x4, 0x200, 0x400
IFA_ADDRESS, IFA_LOCAL = 1, 2
EEXIST = 17


def rta(kind, data):
    length = 4 + len(data)
    return struct.pack("=HH", length, kind) + data + b"\0" * ((4 - length % 4) % 4)


def add(sock, seq, ip, index):
    raw = socket.inet_aton(ip)
    # ifaddrmsg: family, prefix length, flags, scope (universe), index.
    body = struct.pack("=BBBBI", socket.AF_INET, 32, 0, 0, index) + rta(IFA_LOCAL, raw) + rta(IFA_ADDRESS, raw)
    flags = NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_EXCL
    sock.send(struct.pack("=IHHII", 16 + len(body), RTM_NEWADDR, flags, seq, 0) + body)
    while True:
        msg = sock.recv(65536)
        _, kind, _, got, _ = struct.unpack("=IHHII", msg[:16])
        if got != seq:
            continue
        if kind == NLMSG_ERROR:
            err = -struct.unpack("=i", msg[16:20])[0]
            if err not in (0, EEXIST):
                raise OSError(err, "add %s/32 to lo" % ip)
        return


def main():
    sock = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 0)  # NETLINK_ROUTE
    sock.bind((0, 0))
    index = socket.if_nametoindex("lo")
    for seq, ip in enumerate(sys.argv[1:], 1):
        add(sock, seq, ip, index)
    print("ok", flush=True)


main()
