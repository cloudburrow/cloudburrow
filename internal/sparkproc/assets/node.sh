#!/bin/bash
# A Dataproc cluster's node (internal/sparkproc, #2162): trust the
# cluster's CA for the Storage API's TLS, then run the node's Spark
# daemons in the foreground. Roles:
#   master  - the standalone Master and the Spark History Server;
#   single  - a single node cluster's: the Master, a Worker and the
#             History Server;
#   worker  - a Worker of the master CB_MASTER;
#   history - the History Server alone.
set -e
cp "$JAVA_HOME/lib/security/cacerts" /tmp/cb-trust.jks
chmod 600 /tmp/cb-trust.jks
"$JAVA_HOME/bin/keytool" -importcert -noprompt -alias cloudburrow-spark -file /cb/tls/ca.pem \
  -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
home="${SPARK_HOME:-/opt/spark}"
export SPARK_NO_DAEMONIZE=1
host="$(hostname)"

history() {
  SPARK_HISTORY_OPTS="-Dspark.history.fs.logDirectory=file:/cb/history -Dspark.history.ui.port=18080 \
-Dspark.history.fs.update.interval=5s -Dspark.ui.proxyBase=${CB_HISTORY_BASE:-} -Dspark.history.ui.maxApplications=500" \
    "$home/bin/spark-class" org.apache.spark.deploy.history.HistoryServer
}

worker() {
  exec "$home/bin/spark-class" org.apache.spark.deploy.worker.Worker --host "$host" --cores "${CB_CORES:-2}" \
    --memory "${CB_MEMORY:-2g}" --webui-port 8081 "spark://$1:7077"
}

case "$1" in
master)
  history &
  exec "$home/bin/spark-class" org.apache.spark.deploy.master.Master --host "$host" --port 7077 --webui-port 8080
  ;;
single)
  history &
  (sleep 3; worker "$host") &
  exec "$home/bin/spark-class" org.apache.spark.deploy.master.Master --host "$host" --port 7077 --webui-port 8080
  ;;
worker)
  worker "$CB_MASTER"
  ;;
history)
  history
  ;;
*)
  echo "unknown role $1" >&2
  exit 2
  ;;
esac
