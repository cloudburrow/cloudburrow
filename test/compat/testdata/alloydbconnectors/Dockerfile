# The no-egress container of TestAlloyDBConnectorsSendRealSQL
# (test/compat/alloydbconnectors_test.go): Google's AlloyDB Python
# connector, Java connector and Auth Proxy, each pinned, on the base of
# testdata/egress (Node, Python, iptables), plus a JDK and Maven to fetch the
# Java connector's jars. The sink and the probes are mounted at /egress.
FROM node:24.11.1-bookworm-slim@sha256:48abc13a19400ca3985071e287bd405a1d99306770eb81d61202fb6b65cf0b57
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv iptables iproute2 ca-certificates \
    openjdk-17-jdk-headless maven curl \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /py && /py/bin/pip install --no-cache-dir \
    google-cloud-alloydb-connector[pg8000]==1.15.0
COPY pom.xml /java/pom.xml
RUN cd /java && mvn -q -B dependency:copy-dependencies -DoutputDirectory=/java/lib && rm -rf /root/.m2
# The Auth Proxy, checked against the SHA-256 its release page lists
# (github.com/GoogleCloudPlatform/alloydb-auth-proxy/releases/tag/v1.18.0).
ARG TARGETARCH
RUN case "$TARGETARCH" in \
      amd64) sum=2c6f0e36d41929f28c6fda7a78ae3b2ee98b490598b03c0a0bbf7b0ea03bcf75 ;; \
      arm64) sum=eaec142da7592086c09e4c4e775bef137af878386d03c283fc9bb536c1f01ff6 ;; \
      *) echo "no pinned Auth Proxy for $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /usr/local/bin/alloydb-auth-proxy \
      "https://storage.googleapis.com/alloydb-auth-proxy/v1.18.0/alloydb-auth-proxy.linux.$TARGETARCH" \
 && echo "$sum  /usr/local/bin/alloydb-auth-proxy" | sha256sum -c - \
 && chmod 755 /usr/local/bin/alloydb-auth-proxy
