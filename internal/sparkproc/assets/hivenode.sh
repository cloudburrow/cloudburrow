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
  # WebHCat is Jersey 1: with Hive's JAX-RS 2 API jar on its class path
  # every request fails (AbstractMethodError on UriBuilder.uri). It runs
  # from a home of links to Hive's files without that jar; its scripts
  # resolve their own path, so sbin is a copy.
  farm() {
    mkdir -p "$2"
    for e in "$1"/*; do
      n="$(basename "$e")"
      case "$n" in javax.ws.rs-api-*.jar) ;; *) ln -s "$e" "$2/$n" ;; esac
    done
  }
  W=/tmp/webhcat-home
  farm "$HIVE_HOME" "$W"
  for d in lib hcatalog hcatalog/share hcatalog/share/webhcat hcatalog/share/webhcat/svr hcatalog/share/webhcat/svr/lib; do
    rm "$W/$d"
    farm "$HIVE_HOME/$d" "$W/$d"
  done
  rm "$W/hcatalog/sbin"
  cp -r "$HIVE_HOME/hcatalog/sbin" "$W/hcatalog/sbin"
  export HCAT_LOG_DIR=/tmp/webhcat WEBHCAT_LOG_DIR=/tmp/webhcat
  mkdir -p /tmp/webhcat
  HIVE_HOME="$W" HCAT_PREFIX="$W/hcatalog" HADOOP_CLASSPATH=/cb-gcs/gcs-connector.jar "$W/hcatalog/sbin/webhcat_server.sh" foreground &
fi
exec "$HIVE_HOME/bin/hive" --skiphadoopversion --skiphbasecp --service hiveserver2
