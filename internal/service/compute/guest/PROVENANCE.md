# VM image build context (#1963)

Written for CloudBurrow (MIT): the Dockerfiles and every file under `root/`.

At first use, `docker build` on the user's machine pulls these official container images, pinned by index digest (read 2026-11-19 from Docker Hub), and installs the distribution's own packages (systemd, OpenSSH, sudo, curl, iproute2, procps) from that distribution's archive. None of it is vendored into this repository or linked into the CloudBurrow binary; the packages carry their own licences (several GPL) inside the built image only.

| Image | Reference | Source |
|---|---|---|
| Debian 12 | `docker.io/library/debian:12@sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8` | Docker Official Image, https://hub.docker.com/_/debian |
| Ubuntu 24.04 | `docker.io/library/ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8` | Docker Official Image, https://hub.docker.com/_/ubuntu |
| Rocky Linux 9 | `docker.io/rockylinux/rockylinux:9@sha256:bb5755f96bec05ff26165e779ec843d7167544be9e50770508f307019a6d05c4` | The Rocky Enterprise Software Foundation's image, https://hub.docker.com/r/rockylinux/rockylinux |

## Google's Ops Agent (#2139)

Each image also installs Google's Ops Agent **2.73.0** (Apache-2.0; it bundles Fluent Bit and the OpenTelemetry Collector, both Apache-2.0) from Google's own package repository, as Google's `add-google-cloud-ops-agent-repo.sh` adds it: `https://packages.cloud.google.com/apt` `google-cloud-ops-agent-<codename>-all` (packages `2.73.0~debian12`, `2.73.0~ubuntu24.04`), signed by Google's apt key, and `https://packages.cloud.google.com/yum/repos/google-cloud-ops-agent-el9-<arch>-all` (`2.73.0-1.el9`), checked against Google's RPM keys. With it, rsyslog (the agent's default syslog receiver reads its files) and Perl (the metadata server relay) come from the distribution's archive. Source: https://github.com/GoogleCloudPlatform/ops-agent/tree/2.73.0. Nothing of the agent is in this repository; the drop-ins under `root/etc/systemd/system/google-cloud-ops-agent*.service.d/`, `root/usr/lib/cloudburrow/ops-agent` and `metadata-server` are CloudBurrow's (MIT).

## Cloud Router's BGP speaker (#2140)

A router appliance peer's speaker pod runs the Debian 12 image's `root/usr/lib/cloudburrow/vpc` agent beside GoBGP **v3.37.0** (Apache-2.0, https://github.com/osrg/gobgp): `gobgpd` and `gobgp`, which the CLI builds at first use with `go install github.com/osrg/gobgp/v3/cmd/gobgpd@v3.37.0 github.com/osrg/gobgp/v3/cmd/gobgp@v3.37.0` (checked against sum.golang.org) in `docker.io/library/golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191` into a `FROM scratch` image of its own (the Dockerfile is `gobgpDockerfile` in `cmd/cloudburrow/computebgp.go`). Nothing of GoBGP is in this repository, in these images or linked into the CloudBurrow binary.
