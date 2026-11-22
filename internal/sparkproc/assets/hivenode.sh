#!/bin/bash
# A Dataproc cluster's Hive services (internal/sparkproc, #2162), as its
# master runs them: the Hive metastore (thrift, 9083; Derby, its database
# in the cluster's warehouse directory) and HiveServer2 (10000, its web UI
# 10002), and with the Hive WebHCat component WebHCat (50111). The
# container shares the master's network, so they answer at CLUSTER-m.
set -e
export HADOOP_CONF_DIR=/cb-conf HIVE_CONF_DIR=/cb-conf
export HADOOP_CLASSPATH="$TEZ_HOME/*:$TEZ_HOME/lib/*:/cb-gcs/gcs-connector.jar"
export HADOOP_CLIENT_OPTS="${HADOOP_CLIENT_OPTS:-} -Xmx1G"
mkdir -p /cb-warehouse/warehouse /tmp/hive-scratch /tmp/hive-install
if [ ! -d /cb-warehouse/metastore_db ]; then
  "$HIVE_HOME/bin/schematool" -dbType derby -initOrUpgradeSchema
fi
"$HIVE_HOME/bin/hive" --skiphadoopversion --skiphbasecp --service metastore &
for _ in $(seq 1 120); do
  (echo >/dev/tcp/127.0.0.1/9083) 2>/dev/null && break
  sleep 1
done
if [ "${CB_WEBHCAT:-}" = "1" ]; then
  export HCAT_LOG_DIR=/tmp/webhcat WEBHCAT_LOG_DIR=/tmp/webhcat
  mkdir -p /tmp/webhcat
  "$HIVE_HOME/hcatalog/sbin/webhcat_server.sh" foreground &
fi
exec "$HIVE_HOME/bin/hive" --skiphadoopversion --skiphbasecp --service hiveserver2
