package sparkproc

// A Dataproc cluster's YARN (#2162): Hadoop MapReduce and Pig jobs run on
// YARN, as on a Dataproc cluster, not on Hadoop's local job runner. The
// ResourceManager and the MapReduce JobHistory Server run in a container
// of the official apache/hive image (Apache Hadoop 3.3.6) sharing the
// master's network, so they answer at CLUSTER-m:8032, :8088 (the
// ResourceManager's web UI), :10020 and :19888 (the JobHistory Server's),
// as on a Dataproc master. Each worker runs a NodeManager in a container
// of the same image sharing that worker's network (a single node
// cluster's on the master), registered as the worker's host name
// (CLUSTER-w-N), with the mapreduce_shuffle auxiliary service; the
// MapReduce ApplicationMaster and its tasks are YARN containers of the
// NodeManagers. The Capacity Scheduler has the one default queue, as a
// Dataproc cluster's.
//
// A cluster has no HDFS: fs.defaultFS is file:///, and the MapReduce
// staging and done directories (yarn.app.mapreduce.am.staging-dir) are a
// directory of the cluster's mounted at /cb-shared on every node and
// driver, as HDFS is shared by a Dataproc cluster's nodes. A job's gs://
// paths are the instance's Cloud Storage through Google's connector,
// which is on the tasks' class path.
//
// The Component Gateway serves the ResourceManager's UI ("YARN
// ResourceManager", /yarn/) and the JobHistory Server's ("MapReduce Job
// History", /jobhistory/); the cluster relay publishes their ports on the
// host's loopback.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The YARN daemons' web ports on the master: the ResourceManager's, the
// JobHistory Server's and the Timeline Server's (the application history
// server, "YARN Application Timeline").
const (
	yarnWebPort       = "8088"
	jobHistoryWebPort = "19888"
	timelineWebPort   = "8188"
)

// yarnPorts are the YARN web interfaces the cluster relay publishes, by
// their Web key.
var yarnPorts = []struct{ key, port string }{{"yarn", yarnWebPort}, {"jobhistory", jobHistoryWebPort}, {"apphistory", timelineWebPort}}

// yarnApps is the line Hadoop's YARN client logs for each application it
// submits.
var yarnAppLine = regexp.MustCompile(`Submitted application (application_\d+_\d+)`)

// YarnApp is one YARN application a job ran, as the ResourceManager's
// REST API (ws/v1/cluster/apps/ID) reports it.
type YarnApp struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Final    string  `json:"finalStatus"`
	Progress float32 `json:"progress"`
}

// yarnEnabled says whether the cluster runs YARN: every cluster whose
// runner serves the Hadoop image.
func (c *Cluster) yarnEnabled() bool {
	e := c.d.Ecosystem
	return !c.spec.HistoryOnly && e != nil && e.Hive != "" && e.GCS != nil
}

// memoryMB is a Spark memory size (2g, 2048m) in megabytes, 0 if not one.
func memoryMB(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1024, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
	default:
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n * mult
}

// yarnConf are the cluster's yarn-site.xml properties and the YARN
// settings of its mapred-site.xml, the cluster's yarn: properties over
// them.
func (c *Cluster) yarnConf(jars []string) (yarn, mapred map[string]string) {
	cores := c.spec.WorkerCores
	if cores <= 0 {
		cores = 2
	}
	// A NodeManager offers at least 4 GB, so a Spark job's
	// ApplicationMaster and an executor fit on one worker (YARN accounts
	// for memory; it enforces none here).
	mem := memoryMB(c.spec.WorkerMemory)
	if mem < 4096 {
		mem = 4096
	}
	master := c.nodeAlias(-1)
	yarn = map[string]string{
		"yarn.resourcemanager.hostname":                                                 master,
		"yarn.resourcemanager.bind-host":                                                "0.0.0.0",
		"yarn.nodemanager.bind-host":                                                    "0.0.0.0",
		"yarn.nodemanager.aux-services":                                                 "mapreduce_shuffle",
		"yarn.nodemanager.aux-services.mapreduce_shuffle.class":                         "org.apache.hadoop.mapred.ShuffleHandler",
		"yarn.nodemanager.resource.memory-mb":                                           strconv.Itoa(mem),
		"yarn.nodemanager.resource.cpu-vcores":                                          strconv.Itoa(cores),
		"yarn.scheduler.minimum-allocation-mb":                                          "256",
		"yarn.scheduler.maximum-allocation-mb":                                          strconv.Itoa(mem),
		"yarn.nodemanager.vmem-check-enabled":                                           "false",
		"yarn.nodemanager.local-dirs":                                                   "/tmp/yarn-local",
		"yarn.nodemanager.log-dirs":                                                     "/tmp/yarn-logs",
		"yarn.nodemanager.disk-health-checker.max-disk-utilization-per-disk-percentage": "99.9",
		"yarn.nodemanager.env-whitelist":                                                "JAVA_HOME,HADOOP_COMMON_HOME,HADOOP_HDFS_HOME,HADOOP_CONF_DIR,CLASSPATH_PREPEND_DISTCACHE,HADOOP_YARN_HOME,HADOOP_HOME,PATH,LANG,TZ,HADOOP_MAPRED_HOME",
		"yarn.log-aggregation-enable":                                                   "false",
		"yarn.log.server.url":                                                           "http://" + master + ":" + jobHistoryWebPort + "/jobhistory/logs",
		"yarn.resourcemanager.webapp.address":                                           master + ":" + yarnWebPort,
		"yarn.timeline-service.enabled":                                                 "true",
		"yarn.timeline-service.hostname":                                                master,
		"yarn.timeline-service.bind-host":                                               "0.0.0.0",
		"yarn.timeline-service.generic-application-history.enabled":                     "true",
		"yarn.system-metrics-publisher.enabled":                                         "true",
		// The default LevelDB store's JNI library has no linux/arm64 build.
		"yarn.timeline-service.store-class": "org.apache.hadoop.yarn.server.timeline.MemoryTimelineStore",
	}
	cp := "/opt/hadoop/share/hadoop/mapreduce/*:/opt/hadoop/share/hadoop/mapreduce/lib/*:/cb-gcs/gcs-connector.jar"
	for _, j := range jars {
		cp += ":" + j
	}
	mapred = map[string]string{
		"mapreduce.framework.name":                     "yarn",
		"yarn.app.mapreduce.am.staging-dir":            "/cb-shared/staging",
		"mapreduce.jobhistory.address":                 master + ":10020",
		"mapreduce.jobhistory.webapp.address":          master + ":" + jobHistoryWebPort,
		"mapreduce.jobhistory.bind-host":               "0.0.0.0",
		"mapreduce.jobhistory.intermediate-done-dir":   "/cb-shared/history/done_intermediate",
		"mapreduce.jobhistory.done-dir":                "/cb-shared/history/done",
		"mapreduce.application.classpath":              cp,
		"yarn.app.mapreduce.am.env":                    "HADOOP_MAPRED_HOME=/opt/hadoop",
		"mapreduce.map.env":                            "HADOOP_MAPRED_HOME=/opt/hadoop",
		"mapreduce.reduce.env":                         "HADOOP_MAPRED_HOME=/opt/hadoop",
		"yarn.app.mapreduce.am.resource.mb":            "768",
		"yarn.app.mapreduce.am.command-opts":           "-Xmx600m",
		"mapreduce.map.memory.mb":                      "512",
		"mapreduce.reduce.memory.mb":                   "512",
		"mapreduce.map.java.opts":                      "-Xmx400m",
		"mapreduce.reduce.java.opts":                   "-Xmx400m",
		"mapreduce.job.reduce.slowstart.completedmaps": "0.95",
	}
	for k, v := range c.confProps("yarn") {
		yarn[k] = v
	}
	return yarn, mapred
}

// capacityScheduler is capacity-scheduler.xml: the one default queue.
func (c *Cluster) capacityScheduler() map[string]string {
	props := map[string]string{
		"yarn.scheduler.capacity.root.queues":                   "default",
		"yarn.scheduler.capacity.root.default.capacity":         "100",
		"yarn.scheduler.capacity.root.default.maximum-capacity": "100",
		"yarn.scheduler.capacity.maximum-am-resource-percent":   "0.5",
		"yarn.scheduler.capacity.resource-calculator":           "org.apache.hadoop.yarn.util.resource.DefaultResourceCalculator",
	}
	for k, v := range c.confProps("capacity-scheduler") {
		props[k] = v
	}
	return props
}

// yarnMounts are the mounts every YARN node and job driver has: the
// cluster's configuration, the shared staging directory, the jobs' files
// and Google's Cloud Storage connector.
func (c *Cluster) yarnMounts(gcs string) []string {
	return []string{"-v", filepath.Join(c.dir, "conf") + ":/cb-conf:ro", "-v", filepath.Join(c.dir, "shared") + ":/cb-shared",
		"-v", filepath.Join(c.dir, "jobs") + ":/cb-jobs:ro", "-v", gcs + ":/cb-gcs/gcs-connector.jar:ro"}
}

// startYARN starts the ResourceManager and JobHistory Server on the
// master and waits for the ResourceManager's web UI.
func (c *Cluster) startYARN(ctx context.Context) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	args := append([]string{"--user", "0:0"}, c.yarnMounts(gcs)...)
	args = append(args, "-e", "HADOOP_CONF_DIR=/cb-conf", "-e", "HADOOP_CLASSPATH=/cb-gcs/gcs-connector.jar",
		"-e", "YARN_RESOURCEMANAGER_HEAPSIZE=512", "-e", "HADOOP_JOB_HISTORYSERVER_HEAPSIZE=256",
		"--entrypoint", "/bin/bash", e.Hive, "-c",
		"mkdir -p /cb-shared/staging /cb-shared/history && (/opt/hadoop/bin/mapred historyserver &) && "+
			"(YARN_TIMELINESERVER_HEAPSIZE=256 /opt/hadoop/bin/yarn timelineserver &) && exec /opt/hadoop/bin/yarn resourcemanager")
	if err := c.runComponent(ctx, "yarn", "", args...); err != nil {
		return err
	}
	if err := c.waitPort(ctx, c.component("yarn"), yarnWebPort, 3*time.Minute); err != nil {
		return err
	}
	if err := c.waitPort(ctx, c.component("yarn"), jobHistoryWebPort, 3*time.Minute); err != nil {
		return err
	}
	return c.waitPort(ctx, c.component("yarn"), timelineWebPort, 3*time.Minute)
}

// nodeManager is the NodeManager container of the node container node.
func nodeManager(node string) string { return node + "-nm" }

// startNodeManager starts the NodeManager of node (a worker's container,
// or a single node cluster's master), host its host name: a container of
// the cluster's Spark image sharing node's network, so a Spark job's YARN
// containers (its ApplicationMaster, executors and, in cluster mode, its
// driver) run on the image version's Spark, Java, Python and R, as on a
// Dataproc worker. Hadoop, Tez and their Java 8 are the Hive image's,
// mounted read-only (a Docker image mount: nothing copied out of it);
// MapReduce and Tez containers run on that Java, Spark's on the image's
// (spark.yarn.appMasterEnv.JAVA_HOME and spark.executorEnv.JAVA_HOME).
func (c *Cluster) startNodeManager(ctx context.Context, node, host string) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	name := nodeManager(node)
	args := []string{"run", "-d", "--name", name, "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID,
		"--network", "container:" + node, "--user", "0:0",
		"--mount", "type=image,source=" + e.Hive + ",target=/cb-hive",
		"-v", filepath.Join(c.dir, "nodemanager.sh") + ":/cb/nodemanager.sh:ro", "-v", filepath.Join(c.dir, "tls") + ":/cb/tls:ro",
		"-v", c.HistoryDir() + ":/cb-history", "-v", filepath.Join(c.dir, "warehouse") + ":/cb-warehouse",
		"-v", c.jar + ":/cb-connector/spark-bigquery.jar:ro"}
	args = append(args, c.yarnMounts(gcs)...)
	args = append(args, c.lakeMounts...)
	args = append(args, c.dockerClient(true)...)
	args = append(args, "-e", "HADOOP_CONF_DIR=/cb-conf", "-e", "HADOOP_CLASSPATH=/cb-gcs/gcs-connector.jar",
		"-e", "YARN_NODEMANAGER_HEAPSIZE=384", "-e", "YARN_NODEMANAGER_OPTS=-Dyarn.nodemanager.hostname="+host,
		"-e", "PYSPARK_PYTHON=python3", "--entrypoint", "/bin/bash", c.rt.Image, "/cb/nodemanager.sh")
	if out, err := c.d.docker(ctx, args...); err != nil {
		return &Unavailable{Msg: "start the NodeManager of " + host + ": " + out}
	}
	return nil
}

// waitNodeManagers waits for every NodeManager to register with the
// ResourceManager.
func (c *Cluster) waitNodeManagers(ctx context.Context, nodes []string) error {
	for _, n := range nodes {
		if err := c.waitLog(ctx, nodeManager(n), regexp.MustCompile(`Registered with ResourceManager`), 3*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// yarnNodesLocked are the containers whose NodeManagers run: the
// workers, or a single node cluster's master.
func (c *Cluster) yarnNodesLocked() []string {
	if !c.yarnEnabled() {
		return nil
	}
	if c.singleNM {
		return []string{c.master}
	}
	return append([]string(nil), c.workers...)
}

// nodeManagersLocked are the NodeManager containers.
func (c *Cluster) nodeManagersLocked() []string {
	var out []string
	for _, n := range c.yarnNodesLocked() {
		out = append(out, nodeManager(n))
	}
	return out
}

// YarnApps are the YARN applications of a job's driver output, as the
// ResourceManager reports them now.
func (c *Cluster) YarnApps(ctx context.Context, output string) []YarnApp {
	var ids []string
	seen := map[string]bool{}
	for _, m := range yarnAppLine.FindAllStringSubmatch(output, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	if len(ids) == 0 {
		return nil
	}
	addr := c.Web(ctx)["yarn"]
	out := make([]YarnApp, 0, len(ids))
	for _, id := range ids {
		app := YarnApp{ID: id}
		if addr != "" {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ws/v1/cluster/apps/"+id, nil)
			if err == nil {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					var body struct {
						App YarnApp `json:"app"`
					}
					if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && body.App.ID != "" {
						app = body.App
					}
					resp.Body.Close()
				}
			}
		}
		out = append(out, app)
	}
	return out
}

// String names the app for logs.
func (a YarnApp) String() string { return fmt.Sprintf("%s (%s %s)", a.ID, a.State, a.Final) }

// yarnSparkConf sets conf, a Spark job's properties, for Spark on YARN,
// as a Dataproc cluster runs Spark: spark.submit.deployMode client (the
// driver in the job's driver container; the default) or cluster (the
// driver in the ApplicationMaster's YARN container). Spark's jars are the
// NodeManagers' own image's (spark.yarn.jars local:), the staging
// directory is on the shared file system, and the ApplicationMaster and
// executors run on the image's Java. The cluster's and the job's own
// properties win over these; a Spark SQL job runs in client mode, as
// spark-sql's shell runs in no other.
func (c *Cluster) yarnSparkConf(job Job, conf map[string]string) {
	mode := conf["spark.submit.deployMode"]
	if v, ok := job.Properties["spark.submit.deployMode"]; ok {
		mode = v
	}
	if mode != "cluster" || job.Kind == KindSparkSQL {
		mode = "client"
	}
	defaults := map[string]string{
		"spark.yarn.stagingDir":                  "file:///cb-shared/spark-staging",
		"spark.yarn.jars":                        "local:/opt/spark/jars/*",
		"spark.yarn.appMasterEnv.JAVA_HOME":      "/opt/java/openjdk",
		"spark.executorEnv.JAVA_HOME":            "/opt/java/openjdk",
		"spark.yarn.appMasterEnv.PYSPARK_PYTHON": "python3",
		"spark.yarn.am.memory":                   "512m",
		"spark.executor.memory":                  "1g",
		"spark.executor.cores":                   "1",
		"spark.yarn.submit.waitAppCompletion":    "true",
	}
	for k, v := range defaults {
		if _, ok := conf[k]; !ok {
			conf[k] = v
		}
	}
	conf["spark.submit.deployMode"] = mode
	if mode == "cluster" {
		delete(conf, "spark.driver.host")
	}
}
