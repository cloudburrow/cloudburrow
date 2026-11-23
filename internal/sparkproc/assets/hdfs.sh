#!/bin/bash
# A Dataproc cluster's HDFS daemons (internal/sparkproc, #2186), in a
# container of the official apache/hive image (Apache Hadoop 3.3.6, the
# Hadoop the cluster's YARN runs) sharing a node's network, its data on
# the node's own disk (the container's file system, at Dataproc's
# /hadoop/dfs paths), the cluster's configuration HADOOP_CONF_DIR's.
#   namenode - on the master: formats the name directory once (at the
#              cluster's creation), runs the NameNode (RPC 8020, web UI
#              9870) as the hdfs superuser, and once it is out of safe
#              mode makes the directories a Dataproc cluster has in HDFS.
#   datanode - on a primary worker (a single node cluster's master),
#              registered as the node's host name (CB_HOSTNAME); its
#              capacity the node's boot disk (CB_DISK_GB), unless the
#              cluster's hdfs: properties set dfs.datanode.du.reserved.
# With the RANGER component, Ranger's HDFS plugin (its shim and class
# loader, the plugin in ranger-hdfs-plugin-impl beside them) is on the
# NameNode's class path: CB_RANGER_HDFS is its lib directory.
set -euo pipefail
role="$1"
export HADOOP_CONF_DIR=/cb-conf HADOOP_LOG_DIR=/tmp/hadoop-logs HADOOP_USER_NAME=hdfs
export HADOOP_CLASSPATH=/cb-gcs/gcs-connector.jar
hdfs=/opt/hadoop/bin/hdfs
case "$role" in
namenode)
  export HADOOP_HEAPSIZE_MAX=512m
  if [ -n "${CB_RANGER_HDFS:-}" ]; then
    export HADOOP_CLASSPATH="$HADOOP_CLASSPATH:$CB_RANGER_HDFS/*"
  fi
  mkdir -p /hadoop/dfs/name
  if [ ! -f /hadoop/dfs/name/current/VERSION ]; then
    "$hdfs" namenode -format -nonInteractive -force -clusterid "${CB_CLUSTER_ID:?}"
  fi
  "$hdfs" namenode &
  nn=$!
  (
    until "$hdfs" dfsadmin -safemode wait >/dev/null 2>&1; do
      kill -0 "$nn" 2>/dev/null || exit 1
      sleep 2
    done
    # Dataproc's HDFS directories: /tmp and the users' homes, YARN's
    # MapReduce staging and JobHistory directories, and Hive's warehouse.
    for d in /tmp /user /tmp/hadoop-yarn/staging /tmp/hadoop-yarn/staging/history /tmp/hadoop-yarn/staging/history/done_intermediate /tmp/hive; do
      "$hdfs" dfs -mkdir -p "$d"
      "$hdfs" dfs -chmod 1777 "$d"
    done
    for u in root hive spark yarn mapred hdfs; do
      "$hdfs" dfs -mkdir -p "/user/$u"
      "$hdfs" dfs -chown "$u" "/user/$u"
    done
    "$hdfs" dfs -mkdir -p /user/hive/warehouse
    "$hdfs" dfs -chown hive /user/hive/warehouse
    "$hdfs" dfs -chmod 1777 /user/hive/warehouse
    echo "CLOUDBURROW HDFS READY"
  ) &
  wait "$nn"
  ;;
datanode)
  export HADOOP_HEAPSIZE_MAX=384m
  mkdir -p /hadoop/dfs/data
  args=(-D "dfs.datanode.hostname=${CB_HOSTNAME:?}")
  if [ -n "${CB_DISK_GB:-}" ]; then
    # The DataNode's capacity is its disk's: the node's boot disk size,
    # the rest of the file system it is on reserved.
    size=$(df -P -B1 /hadoop/dfs/data | awk 'NR==2 {print $2}')
    want=$((CB_DISK_GB * 1024 * 1024 * 1024))
    if [ "$size" -gt "$want" ]; then
      args+=(-D "dfs.datanode.du.reserved=$((size - want))")
    fi
  fi
  exec "$hdfs" datanode "${args[@]}"
  ;;
*)
  echo "hdfs.sh: unknown role $role" >&2
  exit 2
  ;;
esac
