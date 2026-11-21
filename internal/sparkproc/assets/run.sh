#!/bin/bash
# The Spark container's entrypoint (internal/sparkproc): trust the run's
# CA for the Storage API's TLS, then spark-submit (the image's, or the
# Spark mounted at SPARK_HOME into a custom container).
set -e
cp "$JAVA_HOME/lib/security/cacerts" /tmp/cb-trust.jks
chmod 600 /tmp/cb-trust.jks
"$JAVA_HOME/bin/keytool" -importcert -noprompt -alias cloudburrow-spark -file /cb/tls/ca.pem \
  -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
exec "${SPARK_HOME:-/opt/spark}/bin/spark-submit" "$@"
