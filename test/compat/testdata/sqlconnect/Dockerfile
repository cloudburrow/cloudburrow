# The containers of TestGcloudSQLConnectInContainer (#2208): `gcloud sql
# connect` and Google's Cloud SQL Auth Proxy beside the database clients
# it runs, psql from the official postgres image and mysql from the
# official mysql image, each at the digest dependencies.json pins for Cloud
# SQL's servers (cloudSQLPostgres, cloudSQLMySQL). The gcloud CLI is the
# release unserved.GcloudVersion pins, checked against dependencies.json's
# realTools.googleCloudCli checksums, and the proxy is the release the
# Makefile's cloud-sql-proxy target pins, checked against Google's
# SHA-256; it goes in the SDK's bin, where gcloud looks for it first.
#
# Built with --target postgres or --target mysql.
FROM postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24 AS fetch
RUN apk add --no-cache curl
RUN case "$(uname -m)" in \
      aarch64) a=arm sum=f40acaca49c9774bca035c96dcf7350a13c09007e906cb4832e91dc36ce11497 \
               p=linux.arm64 psum=a58e9e0d4f10f368dbe2e24079597eb005dd9352b43fb18726f7983f913e6c1f ;; \
      x86_64) a=x86_64 sum=42c32b7553c712f71fcf6f124546c0519437f53755f47f9bd0c957998cd6b826 \
               p=linux.amd64 psum=218ae2a18ef8295c48809b73962f326caca1270aad4623f16c4ff5fa1e9f7d3e ;; \
      *) echo "no pinned gcloud archive for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl --retry 5 --retry-all-errors -fsSLo /tmp/gcloud.tar.gz "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-593.0.0-linux-$a.tar.gz" \
 && echo "$sum  /tmp/gcloud.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/gcloud.tar.gz -C /opt && rm /tmp/gcloud.tar.gz \
 && curl --retry 5 --retry-all-errors -fsSLo /opt/google-cloud-sdk/bin/cloud-sql-proxy "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.19.0/cloud-sql-proxy.$p" \
 && echo "$psum  /opt/google-cloud-sdk/bin/cloud-sql-proxy" | sha256sum -c - \
 && chmod +x /opt/google-cloud-sdk/bin/cloud-sql-proxy

# psql, on Alpine, runs gcloud with Alpine's Python (the archive's bundled
# Python is built for glibc).
FROM postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24 AS postgres
RUN apk add --no-cache python3 bash
COPY --from=fetch /opt/google-cloud-sdk /opt/google-cloud-sdk
ENV PATH=/opt/google-cloud-sdk/bin:$PATH CLOUDSDK_PYTHON=python3
ENTRYPOINT []

# mysql, on Oracle Linux, runs gcloud with the archive's bundled Python.
FROM mysql:8.4@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d AS mysql
COPY --from=fetch /opt/google-cloud-sdk /opt/google-cloud-sdk
ENV PATH=/opt/google-cloud-sdk/bin:$PATH
ENTRYPOINT []
