#!/bin/sh
# Starts the RPC services NFSv3 needs (rpcbind, the NSM status monitor
# NFS-Ganesha's lock manager reports to), the system bus NFS-Ganesha
# expects, then NFS-Ganesha with the configuration the CLI passes, base64,
# in GANESHA_CONFIG.
#
# Each share (#2194) is its own ext4 filesystem in a sparse file of the
# volume, /export/disks/SHARE.img, of the share's capacity (SHARES lists
# NAME:BYTES), so a full share answers ENOSPC as on Google. It is reached
# through a loop device with direct I/O, so each of its reads and writes is
# one I/O of the disk beneath, which the container's cgroup limits to the
# instance's IOPS and throughput (IO_MAX, io.max's keys); READ_BPS shapes
# what the server sends, so reads the page cache answers keep the limit
# too. With CRYPT=1 the loop device is opened through dm-crypt
# (aes-xts-plain64) with the share's 512-bit key, which the CLI writes to
# /run/filestore/keys/SHARE (a tmpfs) after the container starts, then
# .ready; the keys are deleted once used. The share's files are
# /export/fs/SHARE/data, its snapshots /export/fs/SHARE/snapshots.
set -eu
mkdir -p /run/dbus /var/run/ganesha /var/lib/nfs/ganesha /etc/ganesha /export/fs /export/disks
printf '%s' "$GANESHA_CONFIG" | base64 -d > /etc/ganesha/ganesha.conf
DM="cbfs-${CB_ID:-$(hostname)}"
SHARES="${SHARES:-}"

log() { echo "filestore-nfs: $*"; }

# release closes every share's filesystem, mapping and loop device.
release() {
  for m in $(awk '$2 ~ "^/export/fs/" {print $2}' /proc/mounts | sort -r); do
    umount "$m" 2>/dev/null || umount -l "$m" 2>/dev/null || true
  done
  for s in $SHARES; do
    n=${s%%:*}
    if [ -e "/dev/mapper/$DM-$n" ]; then cryptsetup close "$DM-$n" 2>/dev/null || dmsetup remove --force "$DM-$n" 2>/dev/null || true; fi
    for l in $(losetup -n -O NAME -j "/export/disks/$n.img" 2>/dev/null); do losetup -d "$l" 2>/dev/null || true; done
  done
}

# wholedisk is the MAJ:MIN of the disk (not partition) under path $1.
wholedisk() {
  d=$(stat -c %d "$1")
  p="/sys/dev/block/$((d >> 8)):$((d & 255))"
  if [ -f "$p/partition" ]; then cat "$(readlink -f "$p")/../dev"; else cat "$p/dev"; fi
}

if [ -n "$SHARES" ]; then
  # A mapping or loop device a killed server left behind.
  release
  if [ "${CRYPT:-}" = 1 ]; then
    i=0
    while [ ! -f /run/filestore/keys/.ready ]; do
      i=$((i + 1))
      if [ "$i" -gt 600 ]; then log "no key was delivered"; exit 1; fi
      sleep 0.2
    done
  fi
  for s in $SHARES; do
    n=${s%%:*}
    bytes=${s#*:}
    img="/export/disks/$n.img"
    fresh=0
    if [ ! -f "$img" ]; then
      truncate -s "$bytes" "$img"
      fresh=1
    elif [ "$(stat -c %s "$img")" -lt "$bytes" ]; then
      truncate -s "$bytes" "$img"
    fi
    loop=$(losetup -f --show --direct-io=on "$img")
    dev=$loop
    if [ "${CRYPT:-}" = 1 ]; then
      cryptsetup open --type plain --cipher aes-xts-plain64 --key-size 512 \
        --key-file "/run/filestore/keys/$n" "$loop" "$DM-$n"
      dev="/dev/mapper/$DM-$n"
    fi
    if [ "$fresh" = 1 ]; then
      mkfs.ext4 -q -m 0 -i 65536 -E lazy_itable_init=1,lazy_journal_init=1,nodiscard "$dev"
    else
      if ! e2fsck -p "$dev" >/dev/null 2>&1; then
        if [ "${CRYPT:-}" = 1 ] && ! dumpe2fs -h "$dev" >/dev/null 2>&1; then
          log "share $n: its key does not open its data"
          exit 1
        fi
        e2fsck -f -y "$dev" >/dev/null 2>&1 || [ $? -le 2 ]
      fi
      have=$(dumpe2fs -h "$dev" 2>/dev/null | awk -F: '/^Block count/ {c=$2} /^Block size/ {s=$2} END {printf "%d", c * s}')
      if [ "$have" != "$bytes" ]; then
        e2fsck -f -y "$dev" >/dev/null 2>&1 || [ $? -le 2 ]
        resize2fs "$dev" "$((bytes / 1024))K" >/dev/null
      fi
    fi
    mkdir -p "/export/fs/$n"
    mount -t ext4 -o noatime,noinit_itable "$dev" "/export/fs/$n"
    mkdir -p "/export/fs/$n/data" "/export/fs/$n/snapshots"
    # A share an earlier release kept as a plain directory of the volume.
    if [ "$fresh" = 1 ] && [ -d "/export/data/$n" ]; then
      cp -a "/export/data/$n/." "/export/fs/$n/data/" && rm -rf "/export/data/$n"
      if [ -d "/export/snapshots/$n" ]; then cp -a "/export/snapshots/$n/." "/export/fs/$n/snapshots/" && rm -rf "/export/snapshots/$n"; fi
    fi
  done
  rm -rf /run/filestore/keys/* /run/filestore/keys/.ready 2>/dev/null || true
  if [ -n "${IO_MAX:-}" ]; then
    echo "$(wholedisk /export/disks) $IO_MAX" > /sys/fs/cgroup/io.max
  fi
fi
if [ "${READ_BPS:-0}" -gt 0 ]; then
  burst=$((READ_BPS / 10))
  if [ "$burst" -lt 262144 ]; then burst=262144; fi
  tc qdisc replace dev eth0 root tbf rate "${READ_BPS}bps" burst "$burst" latency 200ms
fi
# Each export's directory, as the configuration names it.
sed -n 's/^ *Path = \(.*\);$/\1/p' /etc/ganesha/ganesha.conf | while read -r p; do mkdir -p "$p"; done
rm -f /run/dbus/pid /run/rpcbind.lock /run/rpc.statd.pid
dbus-daemon --system --fork
rpcbind -w
rpc.statd --no-notify
/usr/bin/ganesha.nfsd -F -L STDOUT -f /etc/ganesha/ganesha.conf -N NIV_EVENT &
pid=$!
stop() {
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  sync
  release
  exit 0
}
trap stop TERM INT
set +e
wait "$pid"
code=$?
release
exit "$code"
