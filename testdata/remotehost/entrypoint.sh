#!/bin/sh
# sshd in the background, then Docker's own entrypoint as the main process.
#
# --bip: a VM's Docker puts its default bridge on 172.17.0.0/16 and leaves
# 172.18.0.0/16, where the user's machine has kind's network, free. The
# stand-in's own address is on 172.17.0.0/16 (this machine's default
# bridge), so its Docker would take 172.18.0.0/16 for the bridge, and a
# move of an instance whose kind network is 172.18.0.0/16 here was refused
# ("Pool overlaps with other one on this address space"; Landing 171 run
# 46064522048, TestByocFirstRunWalkThrough on the GitHub runner, whose kind
# network is 172.18.0.0/16). The bridge goes on a range no kind network or
# Docker default pool uses instead, and the default pools stay free as on
# a VM.
set -e
/usr/sbin/sshd -E /var/log/sshd.log
exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock --group docker --bip=10.232.250.1/24
