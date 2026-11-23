#!/bin/sh
# Starts the RPC services NFSv3 needs (rpcbind, the NSM status monitor
# NFS-Ganesha's lock manager reports to), the system bus NFS-Ganesha
# expects, then NFS-Ganesha in the foreground with the configuration the
# CLI passes, base64, in GANESHA_CONFIG.
set -eu
mkdir -p /run/dbus /var/run/ganesha /var/lib/nfs/ganesha /etc/ganesha /export/data /export/snapshots
printf '%s' "$GANESHA_CONFIG" | base64 -d > /etc/ganesha/ganesha.conf
# Each export's directory, as the configuration names it.
sed -n 's/^ *Path = \(.*\);$/\1/p' /etc/ganesha/ganesha.conf | while read -r p; do mkdir -p "$p"; done
rm -f /run/dbus/pid /run/rpcbind.lock /run/rpc.statd.pid
dbus-daemon --system --fork
rpcbind -w
rpc.statd --no-notify
exec /usr/bin/ganesha.nfsd -F -L STDOUT -f /etc/ganesha/ganesha.conf -N NIV_EVENT
