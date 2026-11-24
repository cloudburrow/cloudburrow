#!/bin/bash
# A Dataproc cluster's NodeManager (internal/sparkproc, #2162), in a
# container of the cluster's Spark image (its Java, Python and R, which
# Spark's YARN containers run on), with the official apache/hive image
# mounted read-only at /cb-hive: Hadoop, Tez and the Java 8 the
# NodeManager itself, MapReduce and Tez run on, linked at /opt/hadoop and
# /opt/tez as in that image. The node's trust store is made as node.sh
# makes it, for the executors' Storage API TLS.
set -e
ln -sfn /cb-hive/opt/hadoop /opt/hadoop
ln -sfn /cb-hive/opt/tez /opt/tez
spark_java="${CB_SPARK_JAVA:-/opt/java/openjdk}"
cp "$spark_java/lib/security/cacerts" /tmp/cb-trust.jks
chmod 644 /tmp/cb-trust.jks
"$spark_java/bin/keytool" -importcert -noprompt -alias cloudburrow-spark -file /cb/tls/ca.pem \
  -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
export JAVA_HOME=/cb-hive/usr/local/openjdk-8 HADOOP_HOME=/opt/hadoop HADOOP_LOG_DIR=/tmp/hadoop-logs
exec /opt/hadoop/bin/yarn nodemanager
