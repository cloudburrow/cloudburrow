# The no-egress container of TestServedClientsSendNothingOffTheMachine
# (test/compat/egresscontainer_test.go): Python and Node with Google's
# client libraries at the versions docs/security.md measured, plus
# iptables to count connection attempts. The Go probe and the sink are
# mounted at /egress when it runs.
FROM node:24.11.1-bookworm-slim@sha256:48abc13a19400ca3985071e287bd405a1d99306770eb81d61202fb6b65cf0b57
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv iptables iproute2 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /py && /py/bin/pip install --no-cache-dir \
    google-cloud-kms==3.19.0 google-auth==2.62.2 google-api-core==2.44.0
RUN mkdir /node && cd /node && npm init -y >/dev/null \
 && npm install --no-audit --no-fund @google-cloud/kms@6.3.0 google-gax@6.17.0 google-auth-library@11.3.4 @grpc/grpc-js
