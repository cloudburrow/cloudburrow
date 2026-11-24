#!/bin/sh
# sshd in the background, then Docker's own entrypoint as the main process.
set -e
/usr/sbin/sshd -E /var/log/sshd.log
exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock --group docker
