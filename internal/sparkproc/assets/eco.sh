#!/bin/bash
# A Dataproc cluster's Hadoop MapReduce, Hive or Pig job driver
# (internal/sparkproc, #2162), in the official apache/hive image (Hadoop,
# Hive and Tez) on the cluster's network. The image's files are linked at
# the paths a Dataproc image has them (file:///usr/lib/hadoop-mapreduce/
# hadoop-mapreduce-examples.jar), the job's files are in its working
# directory, Google's Cloud Storage connector is on the class path, and the
# cluster's configuration is HADOOP_CONF_DIR's. Then it runs:
#   hadoop ARGS...  - a Hadoop job (jar MAIN [CLASS] ARGS, or CLASS ARGS);
#   pig ARGS...     - Apache Pig, on Hadoop MapReduce's local runner;
#   hive ARGS...    - Beeline against the cluster's HiveServer2.
set -e
kind="$1"
shift
export HADOOP_CONF_DIR=/cb-conf HIVE_CONF_DIR=/cb-conf
mkdir -p /usr/lib/hadoop-mapreduce
ln -sfn /opt/hadoop /usr/lib/hadoop
ln -sfn /opt/hive /usr/lib/hive
[ -d /cb-pig ] && ln -sfn /cb-pig /usr/lib/pig
for f in /opt/hadoop/share/hadoop/mapreduce/*.jar; do
  b="$(basename "$f")"
  ln -sfn "$f" "/usr/lib/hadoop-mapreduce/$b"
  ln -sfn "$f" "/usr/lib/hadoop-mapreduce/$(echo "$b" | sed -E 's/-[0-9][0-9.]*\.jar$/.jar/')"
done
cp=/cb-gcs/gcs-connector.jar
for j in ${CB_JARS:-}; do cp="$cp:$j"; done
export HADOOP_CLASSPATH="$cp"
# file_uris are in the job's working directory; archive_uris extracted
# there, each into a directory of its own name.
mkdir -p /tmp/cb-work
cd /tmp/cb-work
for f in ${CB_FILES:-}; do ln -sfn "$f" "$(basename "$f")"; done
for a in ${CB_ARCHIVES:-}; do
  d="$(basename "$a")"
  mkdir -p "$d"
  case "$a" in
  *.zip | *.jar) (cd "$d" && "$JAVA_HOME/bin/jar" xf "$a" 2>/dev/null || unzip -q "$a") ;;
  *) tar xf "$a" -C "$d" ;;
  esac
done
case "$kind" in
hadoop)
  exec hadoop "$@"
  ;;
pig)
  export PIG_HOME=/cb-pig PIG_CLASSPATH="$HADOOP_CONF_DIR:$cp" HADOOP_HOME=/opt/hadoop
  exec /cb-pig/bin/pig "$@"
  ;;
hive)
  exec beeline "$@"
  ;;
*)
  echo "unknown job kind $kind" >&2
  exit 2
  ;;
esac
