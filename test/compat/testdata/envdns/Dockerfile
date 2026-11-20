# The container of TestEnvResolvesNoGoogleName (test/compat/envdns_test.go):
# the no-egress image of testdata/egress (Python's google-auth, Node's
# google-auth-library and gcp-metadata, iptables), passed as BASE, plus the
# gcloud CLI at the version unserved.GcloudVersion pins and Java's
# google-auth-library-oauth2-http with its probe compiled. The Go probe,
# the sink and the scripts are mounted at /egress when it runs.
ARG BASE
FROM ${BASE}
RUN apt-get update && apt-get install -y --no-install-recommends curl openjdk-17-jdk-headless maven \
 && rm -rf /var/lib/apt/lists/*
RUN case "$(uname -m)" in aarch64) a=arm ;; *) a=x86_64 ;; esac \
 && curl --retry 5 --retry-all-errors -fsSL "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-593.0.0-linux-$a.tar.gz" \
  | tar -xz -C /opt
COPY pom.xml Probe.java /java/
RUN cd /java && mvn -q -B dependency:copy-dependencies -DoutputDirectory=/java/lib \
 && javac -cp '/java/lib/*' -d /java Probe.java && rm -rf /root/.m2
