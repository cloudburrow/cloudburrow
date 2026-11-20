# The container of TestEnvResolvesNoGoogleName (test/compat/envdns_test.go):
# the no-egress image of testdata/egress (Python's google-auth, Node's
# google-auth-library and gcp-metadata, iptables), plus the gcloud CLI at
# the version unserved.GcloudVersion pins and Java's
# google-auth-library-oauth2-http with its probe compiled. The Go probe,
# the sink and the scripts are mounted at /egress when it runs.
#
# The first three steps repeat testdata/egress/Dockerfile word for word, on
# the same base (dependencies.json's testOracles.egressProbeBase), so the
# two images share those layers in Docker's cache; keep them in step.
FROM node:24.11.1-bookworm-slim@sha256:48abc13a19400ca3985071e287bd405a1d99306770eb81d61202fb6b65cf0b57
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv iptables iproute2 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /py && /py/bin/pip install --no-cache-dir \
    google-cloud-kms==3.19.0 google-auth==2.62.2 google-api-core==2.44.0
RUN mkdir /node && cd /node && npm init -y >/dev/null \
 && npm install --no-audit --no-fund @google-cloud/kms@6.3.0 google-gax@6.17.0 google-auth-library@11.3.4 @grpc/grpc-js
RUN apt-get update && apt-get install -y --no-install-recommends curl openjdk-17-jdk-headless maven \
 && rm -rf /var/lib/apt/lists/*
# The archive for this architecture, checked against dependencies.json's
# realTools.googleCloudCli checksums.
RUN case "$(uname -m)" in \
      aarch64) a=arm sum=f40acaca49c9774bca035c96dcf7350a13c09007e906cb4832e91dc36ce11497 ;; \
      x86_64) a=x86_64 sum=42c32b7553c712f71fcf6f124546c0519437f53755f47f9bd0c957998cd6b826 ;; \
      *) echo "no pinned gcloud archive for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl --retry 5 --retry-all-errors -fsSLo /tmp/gcloud.tar.gz "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-593.0.0-linux-$a.tar.gz" \
 && echo "$sum  /tmp/gcloud.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/gcloud.tar.gz -C /opt && rm /tmp/gcloud.tar.gz
COPY pom.xml Probe.java /java/
RUN cd /java && mvn -q -B dependency:copy-dependencies -DoutputDirectory=/java/lib \
 && javac -cp '/java/lib/*' -d /java Probe.java && rm -rf /root/.m2
