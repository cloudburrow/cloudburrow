# The no-egress container of TestServedClientsSendNothingOffTheMachine
# (test/compat/egresscontainer_test.go): Python and Node with Google's
# client libraries at the versions docs/security.md measured, plus
# iptables to count connection attempts. The Go probe and the sink are
# mounted at /egress when it runs.
FROM node:24.11.1-bookworm-slim@sha256:48abc13a19400ca3985071e287bd405a1d99306770eb81d61202fb6b65cf0b57
# The Debian archive at one fixed time through snapshot.debian.org (#2258), so
# a rebuild installs the same package versions and no build depends on a live
# mirror's health; the snapshot's Release files are past their Valid-Until,
# their signatures still checked against the base's keyring. Retried.
RUN printf '%s\n' 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80cloudburrow-snapshot \
 && printf '%s\n' \
      'Types: deb' 'URIs: http://snapshot.debian.org/archive/debian/20261120T000000Z' \
      'Suites: bookworm bookworm-updates' 'Components: main' \
      'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' 'Check-Valid-Until: no' '' \
      'Types: deb' 'URIs: http://snapshot.debian.org/archive/debian-security/20261120T000000Z' \
      'Suites: bookworm-security' 'Components: main' \
      'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' 'Check-Valid-Until: no' \
      > /etc/apt/sources.list.d/debian.sources \
 && rm -f /etc/apt/sources.list
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv iptables iproute2 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /py && /py/bin/pip install --no-cache-dir \
    google-cloud-kms==3.19.0 google-auth==2.62.2 google-api-core==2.44.0
RUN mkdir /node && cd /node && npm init -y >/dev/null \
 && npm install --no-audit --no-fund @google-cloud/kms@6.3.0 google-gax@6.17.0 google-auth-library@11.3.4 @grpc/grpc-js
