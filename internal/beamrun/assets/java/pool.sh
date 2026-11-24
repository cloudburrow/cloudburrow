#!/bin/sh
# A Dataflow job's Java worker pool (internal/beamrun, #2215), in the
# Apache Beam Java SDK's container: Beam's ExternalWorkerService, on the
# SDK harness's jars and the job's staged jars (which the launcher copied
# to /cb/java/jars), with the job's pipeline options (PIPELINE_OPTIONS)
# and the instance's credentials (CbCredentialFactory). It trusts the
# job's CA (/cb/tls/ca.pem), whose certificate the relay answers TLS with
# on BigQuery's port, for BigQueryIO's Storage Read and Write API clients.
set -e
CP="/opt/apache/beam/jars/*:/cb/java/jars/*"
mkdir -p /tmp/cbcred
javac -nowarn -d /tmp/cbcred -cp "$CP" /cb/java/CbCredentialFactory.java
TRUST=""
if [ -f /cb/tls/ca.pem ]; then
  JH="${JAVA_HOME:-$(dirname "$(dirname "$(readlink -f "$(command -v java)")")")}"
  cp "$JH/lib/security/cacerts" /tmp/cb-trust.jks
  chmod 600 /tmp/cb-trust.jks
  keytool -importcert -noprompt -alias cloudburrow-dataflow -file /cb/tls/ca.pem \
    -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
  TRUST="-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks -Djavax.net.ssl.trustStorePassword=changeit"
fi
PIPELINE_OPTIONS="$(cat /cb/java/options.json)"
export PIPELINE_OPTIONS
# shellcheck disable=SC2086
exec java -Xmx2g -XX:+UseParallelGC $TRUST -javaagent:/opt/apache/beam/jars/open-module-agent.jar \
  -cp "/opt/apache/beam/jars/*:/tmp/cbcred:/cb/java/jars/*" org.apache.beam.fn.harness.ExternalWorkerService
