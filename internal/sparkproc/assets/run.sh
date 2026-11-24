#!/bin/bash
# The Spark container's entrypoint (internal/sparkproc): trust the run's
# CA for the Storage API's TLS, then spark-submit (the image's, or the
# Spark mounted at SPARK_HOME into a custom container).
set -e
cp "$JAVA_HOME/lib/security/cacerts" /tmp/cb-trust.jks
chmod 600 /tmp/cb-trust.jks
"$JAVA_HOME/bin/keytool" -importcert -noprompt -alias cloudburrow-spark -file /cb/tls/ca.pem \
  -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
# Google's runtime keeps Spark at /usr/lib/spark, its examples jar
# spark-examples.jar: a batch's file:///usr/lib/spark/ URIs name these.
home="${SPARK_HOME:-/opt/spark}"
uls=/tmp/cb-usr-lib-spark
mkdir -p "$uls/examples/jars"
for f in "$home"/*; do [ "$(basename "$f")" = examples ] || ln -sfn "$f" "$uls/$(basename "$f")" || true; done
for f in "$home"/examples/*; do [ "$(basename "$f")" = jars ] || ln -sfn "$f" "$uls/examples/$(basename "$f")" || true; done
for f in "$home"/examples/jars/*; do ln -sfn "$f" "$uls/examples/jars/$(basename "$f")" || true; done
for f in "$home"/examples/jars/spark-examples_*.jar; do [ -e "$f" ] && ln -sfn "$f" "$uls/examples/jars/spark-examples.jar" || true; done
exec "$home/bin/spark-submit" "$@"
