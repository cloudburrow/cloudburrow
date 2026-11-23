# Filestore's NFS server (#2192): NFS-Ganesha, Debian's package, run as a
# separate program in a container on the kind Docker network, one container
# per Filestore instance. NFS-Ganesha is LGPL-3.0: the CLI links none of it
# and the repo holds none of its code, only this recipe, which installs
# Debian's build at run time on the machine that runs it.
#
# The base is the Docker official debian image, pinned by its multi-arch
# index digest (dependencies.json's filestore.serverBase records the same
# digest); the packages are pinned to one Debian version through
# snapshot.debian.org, so a rebuild installs the same build.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261120T000000Z trixie main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261120T000000Z trixie-updates main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/20261120T000000Z trixie-security main' \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      nfs-ganesha=6.5-5 nfs-ganesha-vfs=6.5-5 rpcbind=1.2.7-1 nfs-common=1:2.8.3-1 dbus=1.16.2-2 netbase=6.5 cryptsetup-bin=2:2.7.5-2 e2fsprogs=1.47.2-3+b12 iproute2=6.15.0-1 \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/filestore-nfs
ENTRYPOINT ["/usr/local/bin/filestore-nfs"]
