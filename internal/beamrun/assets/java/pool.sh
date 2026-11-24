#!/bin/sh
# A Dataflow job's Java worker pool (internal/beamrun, #2215), in the
# Apache Beam Java SDK's container: Beam's ExternalWorkerService, on the
# SDK harness's jars and the job's staged jars (which the launcher copied
# to /cb/java/jars), with the job's pipeline options (PIPELINE_OPTIONS)
# and the instance's credentials (CbCredentialFactory).
set -e
CP="/opt/apache/beam/jars/*:/cb/java/jars/*"
mkdir -p /tmp/cbcred
javac -nowarn -d /tmp/cbcred -cp "$CP" /cb/java/CbCredentialFactory.java
PIPELINE_OPTIONS="$(cat /cb/java/options.json)"
export PIPELINE_OPTIONS
exec java -Xmx2g -XX:+UseParallelGC -javaagent:/opt/apache/beam/jars/open-module-agent.jar \
  -cp "/opt/apache/beam/jars/*:/tmp/cbcred:/cb/java/jars/*" org.apache.beam.fn.harness.ExternalWorkerService
