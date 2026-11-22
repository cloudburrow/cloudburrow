# VM image build context (#1963)

Written for CloudBurrow (MIT): the Dockerfiles and every file under `root/`.

At first use, `docker build` on the user's machine pulls these official container images, pinned by index digest (read 2026-11-19 from Docker Hub), and installs the distribution's own packages (systemd, OpenSSH, sudo, curl, iproute2, procps) from that distribution's archive. None of it is vendored into this repository or linked into the CloudBurrow binary; the packages carry their own licences (several GPL) inside the built image only.

| Image | Reference | Source |
|---|---|---|
| Debian 12 | `docker.io/library/debian:12@sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8` | Docker Official Image, https://hub.docker.com/_/debian |
| Ubuntu 24.04 | `docker.io/library/ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8` | Docker Official Image, https://hub.docker.com/_/ubuntu |
| Rocky Linux 9 | `docker.io/rockylinux/rockylinux:9@sha256:bb5755f96bec05ff26165e779ec843d7167544be9e50770508f307019a6d05c4` | The Rocky Enterprise Software Foundation's image, https://hub.docker.com/r/rockylinux/rockylinux |
