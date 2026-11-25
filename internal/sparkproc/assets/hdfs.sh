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
# On a High Availability cluster (CB_HA set), the NameNode on -m-0
# (CB_HA=active) formats the name directory and the JournalNodes' shared
# edits, the one on -m-1 (CB_HA=standby) copies it (-bootstrapStandby),
# and each runs a ZKFailoverController beside it, which elects the active
# NameNode through the masters' ZooKeeper ensemble.
#   journalnode - on each master of a High Availability cluster.
# A node's disk is mounted at /hadoop, its local SSDs at /mnt/N: with
# CB_SSDS, the DataNode's volumes are each SSD's /mnt/N/hadoop/dfs/data.
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
    if [ "${CB_HA:-}" = standby ]; then
      # The standby's name directory is the active one's, copied once
      # that NameNode answers.
      until "$hdfs" namenode -bootstrapStandby -nonInteractive -force; do
        sleep 3
      done
    else
      "$hdfs" namenode -format -nonInteractive -force -clusterid "${CB_CLUSTER_ID:?}"
    fi
  fi
  if [ -n "${CB_HA:-}" ]; then
    # ZKFC's parent znode, once per ensemble (made again on an ensemble
    # that lost it): exit status 2 is formatZK's refusal where it is
    # there already.
    until rc=0; "$hdfs" zkfc -formatZK -nonInteractive >/tmp/formatzk.log 2>&1 || rc=$?; [ "$rc" = 0 ] || [ "$rc" = 2 ]; do
      sleep 2
    done
  fi
  "$hdfs" namenode &
  nn=$!
  if [ -n "${CB_HA:-}" ]; then
    (
      while kill -0 "$nn" 2>/dev/null; do
        "$hdfs" zkfc || true
        sleep 2
      done
    ) &
  fi
  (
    if [ -n "${CB_HA:-}" ]; then
      # Through the nameservice: the active NameNode, out of safe mode.
      until "$hdfs" dfs -mkdir -p /tmp >/dev/null 2>&1; do
        kill -0 "$nn" 2>/dev/null || exit 1
        sleep 2
      done
    fi
    until "$hdfs" dfsadmin -safemode wait >/dev/null 2>&1 || [ -n "${CB_HA:-}" ]; do
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
journalnode)
  export HADOOP_HEAPSIZE_MAX=256m
  mkdir -p /hadoop/dfs/journalnode
  exec "$hdfs" journalnode
  ;;
datanode)
  export HADOOP_HEAPSIZE_MAX=384m
  data=/hadoop/dfs/data
  args=(-D "dfs.datanode.hostname=${CB_HOSTNAME:?}")
  if [ -n "${CB_SSDS:-}" ]; then
    # Each local SSD a volume, as Dataproc mounts them.
    dirs=()
    for i in $(seq 1 "$CB_SSDS"); do
      mkdir -p "/mnt/$i/hadoop/dfs/data"
      dirs+=("file:///mnt/$i/hadoop/dfs/data")
    done
    data=/mnt/1/hadoop/dfs/data
    args+=(-D "dfs.datanode.data.dir=$(IFS=,; echo "${dirs[*]}")")
  fi
  mkdir -p "$data"
  if [ -n "${CB_DISK_GB:-}" ]; then
    # Each volume's capacity is its disk's: the node's boot disk size, or
    # a local SSD's, the rest of the file system it is on reserved. On a
    # file system smaller than the disk the reservation is negative: the
    # DataNode's capacity is the file system's size less the reservation
    # (FsVolumeImpl.getCapacity), so still the disk's, while its available
    # space stays what the file system has free (getAvailable takes the
    # smaller, and a negative reservation leaves none of the free space
    # reserved).
    size=$(df -P -B1 "$data" | awk 'NR==2 {print $2}')
    want=$((CB_DISK_GB * 1024 * 1024 * 1024))
    if [ "$size" -ne "$want" ]; then
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
