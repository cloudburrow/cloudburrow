package sparkproc

// A Dataproc cluster's other engines (#2162): Hadoop MapReduce, Hive,
// Pig, Trino and Flink jobs, and the optional components, as containers
// of their projects' official images beside the cluster's Spark nodes.
//
// Hadoop, Hive and Pig run in the official apache/hive image (Apache
// Hadoop 3.3.6, Hive 4.0.1 and Tez, on Java 8), with Google's Cloud
// Storage connector for Hadoop on the class path, so a job's gs:// paths
// are the instance's Cloud Storage: the connector's storage root URL is
// the cluster relay's (host.docker.internal:4080 on the cluster's
// network, relayed to the instance's Cloud Storage port). A Hadoop or Pig
// job is a driver container on the cluster's network, its MapReduce on
// Hadoop's local job runner; a Hive job is Beeline in a driver container
// against the cluster's HiveServer2, which with the Hive metastore runs in
// a container sharing the master's network (so they answer at
// CLUSTER-m:10000 and :9083, as on a Dataproc master), started with the
// first Hive job or with a component that needs the metastore. Apache Pig
// is the Apache release's binary tarball, mounted into the driver.
//
// Dataproc runs these on YARN and HDFS; a local cluster runs no YARN
// ResourceManager or HDFS NameNode: fs.defaultFS is the driver's local
// file system, and the Hive warehouse is a directory of the cluster's.
//
// The optional components run in containers sharing the master's network,
// as a Dataproc master runs them: Trino (the coordinator, 8060, its hive
// catalog the cluster's metastore), Flink (a session cluster's JobManager,
// REST 8081, and a TaskManager), Jupyter (JupyterLab, 8123), Zeppelin
// (8080), ZooKeeper (2181), Solr (8983) and Hive WebHCat (50111, in the
// Hive services' container). The cluster relay publishes their web ports
// on the host's loopback for the Component Gateway.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Ecosystem is what a cluster's other engines run on.
type Ecosystem struct {
	// Hive is the official apache/hive image, by digest: Hadoop, Hive
	// and Tez.
	Hive string
	// GCS returns Google's Cloud Storage connector for Hadoop (its
	// shaded jar) on the host, fetching it on the first use; Pig the
	// Apache Pig release's home directory on the host.
	GCS, Pig func(ctx context.Context) (string, error)
	// Trino, Flink, Zookeeper, Solr and Zeppelin are the official images
	// of those components, by digest ("": not served).
	Trino, Flink, Zookeeper, Solr, Zeppelin string
	// Jupyter returns the Jupyter component's image for a cluster's
	// Spark image (built on it, so a notebook's Python is its
	// executors'), or nil when not served.
	Jupyter func(ctx context.Context, sparkImage string) (string, error)
	// Lake returns the host paths of a table format component's jars
	// (ComponentDelta, ComponentIceberg, ComponentHudi) for an image
	// version (MAJOR.MINOR), fetching them on the first use; iceberg is
	// dataproc:dataproc.iceberg.version ("" for the default).
	Lake func(ctx context.Context, component, imageVersion, iceberg string) ([]string, error)
}

// The optional components (SoftwareConfig.optional_components) a
// cluster runs.
const (
	ComponentHiveWebHCat = "HIVE_WEBHCAT"
	ComponentTrino       = "TRINO"
	ComponentFlink       = "FLINK"
	ComponentJupyter     = "JUPYTER"
	ComponentZeppelin    = "ZEPPELIN"
	ComponentZookeeper   = "ZOOKEEPER"
	ComponentSolr        = "SOLR"
	ComponentPig         = "PIG"
)

// componentPorts are the components' web ports on the master, and the
// port the cluster relay listens for each on (its own 8080 and 8443 are
// the connector's).
var componentPorts = map[string]struct{ key, listen, port string }{
	ComponentTrino:       {"trino", "8060", "8060"},
	ComponentFlink:       {"flink", "8081", "8081"},
	ComponentJupyter:     {"jupyter", "8123", "8123"},
	ComponentZeppelin:    {"zeppelin", "18090", "8080"},
	ComponentSolr:        {"solr", "8983", "8983"},
	ComponentHiveWebHCat: {"webhcat", "50111", "50111"},
}

// gcsPort is the port the cluster relay serves the instance's Cloud
// Storage on, on its restHost alias.
const gcsPort = "4080"

func (c *Cluster) has(component string) bool {
	for _, x := range c.spec.Components {
		if x == component {
			return true
		}
	}
	return false
}

// eco is the runner's Ecosystem, or an *Unavailable.
func (c *Cluster) eco(what string) (*Ecosystem, error) {
	e := c.d.Ecosystem
	if e == nil || e.Hive == "" || e.GCS == nil {
		return nil, &Unavailable{Msg: "the Spark runner serves no " + what}
	}
	return e, nil
}

// confProps are the cluster's properties of a configuration file's
// prefix (core:, mapred:, hive:, pig:), the prefix dropped.
func (c *Cluster) confProps(prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range c.spec.ConfProperties {
		if rest, ok := strings.CutPrefix(k, prefix+":"); ok {
			out[rest] = v
		}
	}
	return out
}

func siteXML(props map[string]string) []byte {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\"?>\n<configuration>\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  <property><name>%s</name><value>%s</value></property>\n", html.EscapeString(k), html.EscapeString(props[k]))
	}
	b.WriteString("</configuration>\n")
	return []byte(b.String())
}

// writeConf writes the cluster's Hadoop, Hive and Tez configuration to
// dir, job (a job's properties, or nil) over the cluster's.
func (c *Cluster) writeConf(dir string, job map[string]string, jars []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	core := map[string]string{
		"fs.defaultFS":                      "file:///",
		"fs.gs.impl":                        "com.google.cloud.hadoop.fs.gcs.GoogleHadoopFileSystem",
		"fs.AbstractFileSystem.gs.impl":     "com.google.cloud.hadoop.fs.gcs.GoogleHadoopFS",
		"fs.gs.project.id":                  c.spec.Project,
		"fs.gs.storage.root.url":            "http://" + restHost + ":" + gcsPort + "/",
		"fs.gs.auth.service.account.enable": "false",
		"fs.gs.auth.null.enable":            "true",
		"hadoop.tmp.dir":                    "/tmp/hadoop",
	}
	mapred := map[string]string{"mapreduce.framework.name": "local", "mapreduce.jobtracker.staging.root.dir": "/tmp/hadoop/staging"}
	hive := map[string]string{
		"hive.server2.enable.doAs":                              "false",
		"hive.tez.exec.inplace.progress":                        "false",
		"hive.exec.scratchdir":                                  "/tmp/hive-scratch",
		"hive.user.install.directory":                           "/tmp/hive-install",
		"tez.runtime.optimize.local.fetch":                      "true",
		"hive.exec.submit.local.task.via.child":                 "false",
		"tez.local.mode":                                        "true",
		"hive.execution.engine":                                 "tez",
		"hive.metastore.warehouse.dir":                          "file:///cb-warehouse/warehouse",
		"hive.metastore.uris":                                   "thrift://" + c.nodeAlias(-1) + ":9083",
		"javax.jdo.option.ConnectionURL":                        "jdbc:derby:;databaseName=/cb-warehouse/metastore_db;create=true",
		"metastore.metastore.event.db.notification.api.auth":    "false",
		"hive.metastore.event.db.notification.api.auth":         "false",
		"datanucleus.schema.autoCreateAll":                      "false",
		"hive.server2.webui.host":                               "0.0.0.0",
		"hive.server2.thrift.bind.host":                         "0.0.0.0",
		"templeton.hive.properties":                             "hive.metastore.uris=thrift://" + c.nodeAlias(-1) + ":9083",
		"hive.metastore.schema.verification":                    "false",
		"hive.server2.logging.operation.enabled":                "true",
		"hive.server2.logging.operation.log.location":           "/tmp/hive-oplogs",
		"hive.exec.local.scratchdir":                            "/tmp/hive-local",
		"hive.downloaded.resources.dir":                         "/tmp/hive-resources",
		"hive.querylog.location":                                "/tmp/hive-querylog",
		"hive.server2.thrift.port":                              "10000",
		"hive.server2.webui.port":                               "10002",
		"hive.metastore.disallow.incompatible.col.type.changes": "false",
	}
	var yarn map[string]string
	if c.yarnEnabled() {
		var m map[string]string
		yarn, m = c.yarnConf(jars)
		for k, v := range m {
			mapred[k] = v
		}
	}
	for k, v := range c.confProps("core") {
		core[k] = v
	}
	for k, v := range c.confProps("mapred") {
		mapred[k] = v
	}
	for k, v := range c.confProps("hive") {
		hive[k] = v
	}
	for k, v := range job {
		mapred[k] = v
	}
	files := map[string]map[string]string{"core-site.xml": core, "mapred-site.xml": mapred, "hive-site.xml": hive,
		"tez-site.xml": {"tez.local.mode": "true", "tez.runtime.optimize.local.fetch": "true", "tez.ignore.lib.uris": "true"}}
	if yarn != nil {
		files["yarn-site.xml"] = yarn
		files["capacity-scheduler.xml"] = c.capacityScheduler()
	}
	for name, props := range files {
		if err := os.WriteFile(filepath.Join(dir, name), siteXML(props), 0o644); err != nil {
			return err
		}
	}
	// Pig in local mode builds its Hadoop configuration from its
	// properties, not HADOOP_CONF_DIR's, so they carry core-site's and
	// mapred-site's (the Cloud Storage connector's among them).
	pig := map[string]string{}
	for _, m := range []map[string]string{core, mapred, c.confProps("pig")} {
		for k, v := range m {
			pig[k] = v
		}
	}
	var pb strings.Builder
	for _, k := range sortedKeys(pig) {
		fmt.Fprintf(&pb, "%s=%s\n", k, pig[k])
	}
	return os.WriteFile(filepath.Join(dir, "pig.properties"), []byte(pb.String()), 0o644)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// component is a component container's name.
func (c *Cluster) component(name string) string { return c.master + "-" + name }

// runComponent runs a component's container in the master's network
// (or node's, when set), detached; it is the cluster's to stop, start and
// remove.
func (c *Cluster) runComponent(ctx context.Context, name, node string, args ...string) error {
	if node == "" {
		node = c.master
	}
	full := c.component(name)
	run := append([]string{"run", "-d", "--name", full, "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID,
		"--network", "container:" + node}, args...)
	if out, err := c.d.docker(ctx, run...); err != nil {
		return &Unavailable{Msg: "start the cluster's " + name + ": " + out}
	}
	c.mu.Lock()
	c.extras = append(c.extras, full)
	c.mu.Unlock()
	return nil
}

// waitPort waits for a TCP port of the master's network to answer, from
// inside container name.
func (c *Cluster) waitPort(ctx context.Context, name, port string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		if _, err := c.d.docker(ctx, "exec", name, "bash", "-c", "echo > /dev/tcp/127.0.0.1/"+port); err == nil {
			return nil
		}
		if st, _ := c.d.docker(ctx, "inspect", "-f", "{{.State.Running}}", name); st == "false" {
			logs, _ := c.d.docker(ctx, "logs", "--tail", "30", name)
			return fmt.Errorf("the cluster's %s stopped: %s", name, logs)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			logs, _ := c.d.docker(ctx, "logs", "--tail", "30", name)
			return fmt.Errorf("the cluster's %s did not answer on port %s: %s", name, port, logs)
		}
		time.Sleep(time.Second)
	}
}

// ensureHive starts the cluster's Hive services, once, and waits for
// HiveServer2.
func (c *Cluster) ensureHive(ctx context.Context) error {
	c.hiveMu.Lock()
	defer c.hiveMu.Unlock()
	name := c.component("hive")
	if c.hiveUp {
		return c.waitPort(ctx, name, "10000", 5*time.Minute)
	}
	e, err := c.eco("Hive")
	if err != nil {
		return err
	}
	jar, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	webhcat := "0"
	if c.has(ComponentHiveWebHCat) {
		webhcat = "1"
	}
	if err := c.runComponent(ctx, "hive", "", "--user", "1000:1000",
		"-v", filepath.Join(c.dir, "conf")+":/cb-conf:ro", "-v", filepath.Join(c.dir, "warehouse")+":/cb-warehouse",
		"-v", jar+":/cb-gcs/gcs-connector.jar:ro", "-v", filepath.Join(c.dir, "hivenode.sh")+":/cb/hivenode.sh:ro",
		"-v", filepath.Join(c.dir, "jobs")+":/cb-jobs:ro", "-e", "CB_WEBHCAT="+webhcat,
		"--entrypoint", "/bin/bash", e.Hive, "/cb/hivenode.sh"); err != nil {
		return err
	}
	c.hiveUp = true
	return c.waitPort(ctx, name, "10000", 5*time.Minute)
}

// startComponents starts the cluster's optional components.
func (c *Cluster) startComponents(ctx context.Context) error {
	e := c.d.Ecosystem
	need := func(image, what string) error {
		if e == nil || image == "" {
			return &Unavailable{Msg: "the Spark runner serves no " + what + " image"}
		}
		return nil
	}
	if c.has(ComponentHiveWebHCat) || c.has(ComponentTrino) {
		if err := c.ensureHive(ctx); err != nil {
			return err
		}
		if c.has(ComponentHiveWebHCat) {
			if err := c.waitPort(ctx, c.component("hive"), "50111", 3*time.Minute); err != nil {
				return err
			}
		}
	}
	if c.has(ComponentTrino) {
		if err := need(e.Trino, "Trino"); err != nil {
			return err
		}
		if err := c.writeTrino(); err != nil {
			return err
		}
		t := filepath.Join(c.dir, "trino")
		if err := c.runComponent(ctx, "trino", "", "-v", filepath.Join(t, "config.properties")+":/etc/trino/config.properties:ro",
			"-v", filepath.Join(t, "jvm.config")+":/etc/trino/jvm.config:ro", "-v", filepath.Join(t, "catalog")+":/etc/trino/catalog:ro",
			"-v", filepath.Join(c.dir, "warehouse")+":/cb-warehouse", e.Trino); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component("trino"), regexp.MustCompile(`SERVER STARTED`), 5*time.Minute); err != nil {
			return err
		}
	}
	if c.has(ComponentFlink) {
		if err := need(e.Flink, "Flink"); err != nil {
			return err
		}
		props := c.flinkProperties()
		if err := c.runComponent(ctx, "flink", "", "-e", "FLINK_PROPERTIES="+props, e.Flink, "jobmanager"); err != nil {
			return err
		}
		// The TaskManager on the first worker (the master of a single
		// node cluster).
		node := c.master
		c.mu.Lock()
		if len(c.workers) > 0 {
			node = c.workers[0]
		}
		c.mu.Unlock()
		if err := c.runComponent(ctx, "flink-tm", node, "-e", "FLINK_PROPERTIES="+props, e.Flink, "taskmanager"); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component("flink"), regexp.MustCompile(`Rest endpoint listening at|Web frontend listening at`), 3*time.Minute); err != nil {
			return err
		}
	}
	if c.has(ComponentZookeeper) {
		if err := need(e.Zookeeper, "ZooKeeper"); err != nil {
			return err
		}
		if err := c.runComponent(ctx, "zookeeper", "", "-e", "ZOO_4LW_COMMANDS_WHITELIST=ruok,stat,srvr,mntr", e.Zookeeper); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component("zookeeper"), regexp.MustCompile(`binding to port|Started AdminServer|PrepRequestProcessor`), 3*time.Minute); err != nil {
			return err
		}
	}
	if c.has(ComponentSolr) {
		if err := need(e.Solr, "Solr"); err != nil {
			return err
		}
		if err := c.runComponent(ctx, "solr", "", "-e", "SOLR_HEAP=512m", e.Solr); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component("solr"), regexp.MustCompile(`Started Solr server|o.e.j.s.Server Started`), 5*time.Minute); err != nil {
			return err
		}
	}
	if c.has(ComponentZeppelin) {
		if err := need(e.Zeppelin, "Zeppelin"); err != nil {
			return err
		}
		if err := c.startZeppelin(ctx, e.Zeppelin); err != nil {
			return err
		}
	}
	if c.has(ComponentJupyter) {
		if e == nil || e.Jupyter == nil {
			return &Unavailable{Msg: "the Spark runner serves no Jupyter image"}
		}
		image, err := e.Jupyter(ctx, c.rt.Image)
		if err != nil {
			return &Unavailable{Msg: "build the Jupyter component's image: " + err.Error()}
		}
		if err := os.MkdirAll(filepath.Join(c.dir, "notebooks"), 0o777); err != nil {
			return err
		}
		_ = os.Chmod(filepath.Join(c.dir, "notebooks"), 0o777)
		if err := c.runComponent(ctx, "jupyter", "", "-v", filepath.Join(c.dir, "notebooks")+":/cb-notebooks",
			"-v", c.HistoryDir()+":/cb-history", "-v", c.jar+":/cb-connector/spark-bigquery.jar:ro",
			"-v", filepath.Join(c.dir, "tls")+":/cb/tls:ro",
			"-e", "CB_MASTER_URL=spark://"+c.nodeAlias(-1)+":7077", "-e", "CB_PROJECT="+c.spec.Project,
			"-e", "CB_BASE_URL="+c.spec.GatewayBase+"jupyter/", image); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component("jupyter"), regexp.MustCompile(`Jupyter Server .* is running at|is running at:`), 3*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// flinkProperties are the Flink session cluster's configuration
// (FLINK_PROPERTIES, flink-conf.yaml's lines).
func (c *Cluster) flinkProperties() string {
	cores := c.spec.WorkerCores
	if cores <= 0 {
		cores = 2
	}
	props := map[string]string{
		"jobmanager.rpc.address":                 c.nodeAlias(-1),
		"jobmanager.bind-host":                   "0.0.0.0",
		"taskmanager.bind-host":                  "0.0.0.0",
		"rest.address":                           c.nodeAlias(-1),
		"rest.bind-address":                      "0.0.0.0",
		"rest.port":                              "8081",
		"taskmanager.numberOfTaskSlots":          fmt.Sprint(cores),
		"jobmanager.memory.process.size":         "1024m",
		"taskmanager.memory.process.size":        "1536m",
		"parallelism.default":                    "1",
		"web.submit.enable":                      "false",
		"execution.target":                       "remote",
		"jobmanager.archive.fs.dir":              "file:///tmp/flink-completed-jobs",
		"historyserver.archive.fs.dir":           "file:///tmp/flink-completed-jobs",
		"rest.flamegraph.enabled":                "false",
		"taskmanager.memory.network.min":         "64mb",
		"taskmanager.memory.network.max":         "64mb",
		"taskmanager.memory.managed.size":        "128mb",
		"jobmanager.execution.failover-strategy": "region",
	}
	for k, v := range c.confProps("flink") {
		props[k] = v
	}
	var b strings.Builder
	for _, k := range sortedKeys(props) {
		fmt.Fprintf(&b, "%s: %s\n", k, props[k])
	}
	return b.String()
}

// writeTrino writes the Trino coordinator's configuration: one node
// that schedules work on itself, on port 8060 as the Trino component's,
// its catalogs tpch, tpcds, memory and hive (the cluster's metastore).
func (c *Cluster) writeTrino() error {
	dir := filepath.Join(c.dir, "trino")
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o755); err != nil {
		return err
	}
	cfg := map[string]string{"coordinator": "true", "node-scheduler.include-coordinator": "true", "http-server.http.port": "8060",
		"discovery.uri": "http://localhost:8060", "query.max-memory": "512MB", "query.max-memory-per-node": "256MB"}
	for k, v := range c.confProps("trino") {
		cfg[k] = v
	}
	var b strings.Builder
	for _, k := range sortedKeys(cfg) {
		fmt.Fprintf(&b, "%s=%s\n", k, cfg[k])
	}
	jvm := "-server\n-Xmx1G\n-XX:G1HeapRegionSize=32M\n-XX:+ExplicitGCInvokesConcurrent\n-XX:+ExitOnOutOfMemoryError\n" +
		"-XX:-OmitStackTraceInFastThrow\n-XX:ReservedCodeCacheSize=256M\n-XX:PerMethodRecompilationCutoff=10000\n" +
		"-XX:PerBytecodeRecompilationCutoff=10000\n-Djdk.attach.allowAttachSelf=true\n-Djdk.nio.maxCachedBufferSize=2000000\n"
	catalogs := map[string]string{
		"tpch.properties":   "connector.name=tpch\n",
		"tpcds.properties":  "connector.name=tpcds\n",
		"memory.properties": "connector.name=memory\n",
		"hive.properties": "connector.name=hive\nhive.metastore.uri=thrift://" + c.nodeAlias(-1) + ":9083\n" +
			"hive.non-managed-table-writes-enabled=true\nhive.storage-format=ORC\n",
	}
	if err := os.WriteFile(filepath.Join(dir, "config.properties"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "jvm.config"), []byte(jvm), 0o644); err != nil {
		return err
	}
	for name, body := range catalogs {
		if err := os.WriteFile(filepath.Join(dir, "catalog", name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// submitEco runs a Hadoop, Hive, Pig, Trino or Flink job.
func (c *Cluster) submitEco(ctx context.Context, job Job) (Result, error) {
	e := c.d.Ecosystem
	switch job.Kind {
	case KindTrino:
		if !c.has(ComponentTrino) {
			return Result{}, errors.New("the cluster does not run the Trino component")
		}
		if e == nil || e.Trino == "" {
			return Result{}, &Unavailable{Msg: "the Spark runner serves no Trino image"}
		}
	case KindFlink:
		if !c.has(ComponentFlink) {
			return Result{}, errors.New("the cluster does not run the Flink component")
		}
		if e == nil || e.Flink == "" {
			return Result{}, &Unavailable{Msg: "the Spark runner serves no Flink image"}
		}
	default:
		if _, err := c.eco("Hadoop"); err != nil {
			return Result{}, err
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(c.dir, "jobs"), "job-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	a, err := writeJob(dir, job)
	if err != nil {
		return Result{}, err
	}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			mode := os.FileMode(0o644)
			if info.IsDir() {
				mode = 0o755
			}
			_ = os.Chmod(p, mode)
		}
		return nil
	})
	// The job's files at the same path in the driver and in the
	// cluster's Hive services (ADD JAR of a Hive job's jars).
	mount := "/cb-jobs/" + filepath.Base(dir)
	rebase := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "/cb/"); ok {
			return mount + "/" + rest
		}
		return p
	}
	main := rebase(a.main)
	var jars, files, archives []string
	for _, j := range a.jars {
		jars = append(jars, rebase(j))
	}
	for _, f := range a.files {
		files = append(files, rebase(f))
	}
	for _, f := range a.archiv {
		archives = append(archives, rebase(f))
	}
	suffix := nameSafe.ReplaceAllString(strings.ToLower(job.SparkJobID), "-")
	if len(suffix) > 40 {
		suffix = suffix[len(suffix)-40:]
	}
	driver := c.master[:len(c.master)-2] + "-d-" + strings.Trim(suffix, "-") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
	netArgs, _ := c.driverNodeNetwork(job, driver)
	run := append(append([]string{"run", "--name", driver, "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID}, netArgs...),
		"-v", dir+":"+mount+":ro", "--user", "0:0")
	vars := sortedKeys(job.QueryVariables)
	props := sortedKeys(job.Properties)
	switch job.Kind {
	case KindHadoop, KindPig, KindHive:
		jar, err := e.GCS(ctx)
		if err != nil {
			return Result{}, &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
		}
		conf := filepath.Join(dir, "conf")
		jobProps := map[string]string{}
		for k, v := range job.Properties {
			jobProps[k] = v
		}
		// file_uris and archive_uris are distributed to the tasks' working
		// directories as -files and -archives would (JobResourceUploader
		// reads tmpfiles and tmparchives).
		if c.yarnEnabled() && job.Kind == KindHadoop {
			if s := fileURIs(files); s != "" && jobProps["tmpfiles"] == "" {
				jobProps["tmpfiles"] = s
			}
			if s := fileURIs(archives); s != "" && jobProps["tmparchives"] == "" {
				jobProps["tmparchives"] = s
			}
		}
		if c.yarnEnabled() && job.Kind == KindPig {
			// Pig sets each input's file system's working directory to the
			// job's (PigInputFormat.getSplits), which on a cluster without
			// HDFS is file:/tmp/cb-work: a gs:// input's connector refuses
			// a path of another scheme or bucket. The job's working
			// directory is /user/root with no scheme, as Dataproc's
			// (hdfs:///user/root) is for each file system; and Pig's
			// intermediate files (DUMP's among them) are in the directory
			// every node shares.
			if jobProps["mapreduce.job.working.dir"] == "" {
				jobProps["mapreduce.job.working.dir"] = "/user/root"
			}
			if jobProps["pig.temp.dir"] == "" {
				jobProps["pig.temp.dir"] = "/cb-shared/pig-tmp"
			}
		}
		if err := c.writeConf(conf, jobProps, jars); err != nil {
			return Result{}, err
		}
		_ = filepath.Walk(conf, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
		if c.yarnEnabled() {
			run = append(run, "-v", filepath.Join(c.dir, "shared")+":/cb-shared")
		}
		run = append(run, "-v", conf+":/cb-conf:ro", "-v", jar+":/cb-gcs/gcs-connector.jar:ro",
			"-v", filepath.Join(c.dir, "eco.sh")+":/cb/eco.sh:ro",
			"-e", "CB_JARS="+strings.Join(jars, " "), "-e", "CB_FILES="+strings.Join(files, " "), "-e", "CB_ARCHIVES="+strings.Join(archives, " "))
		var args []string
		switch job.Kind {
		case KindHadoop:
			args = []string{"hadoop"}
			switch {
			case main != "" && strings.HasSuffix(main, ".jar"):
				args = append(args, "jar", main)
				if job.MainClass != "" {
					args = append(args, job.MainClass)
				}
			case job.MainClass != "":
				args = append(args, job.MainClass)
			default:
				return Result{Failed: true, Message: "hadoop_job needs main_jar_file_uri or main_class"}, nil
			}
			args = append(args, job.Args...)
		case KindPig:
			if e.Pig == nil {
				return Result{}, &Unavailable{Msg: "the Spark runner serves no Apache Pig"}
			}
			pig, err := e.Pig(ctx)
			if err != nil {
				return Result{}, &Unavailable{Msg: "Apache Pig is not available: " + err.Error()}
			}
			run = append(run, "-v", pig+":/cb-pig:ro")
			mode := "local"
			if c.yarnEnabled() {
				mode = "mapreduce"
			}
			args = []string{"pig", "-x", mode, "-P", "/cb-conf/pig.properties"}
			for _, k := range vars {
				args = append(args, "-param", k+"="+job.QueryVariables[k])
			}
			if job.ContinueOnFailure {
				args = append(args, "-stop_on_failure", "false")
			}
			if len(jars) > 0 {
				args = append(args, "-Dpig.additional.jars="+strings.Join(jars, ":"))
			}
			args = append(args, "-f", main)
		case KindHive:
			if err := c.ensureHive(ctx); err != nil {
				return Result{}, err
			}
			args = []string{"hive", "-u", "jdbc:hive2://" + c.nodeAlias(-1) + ":10000/default", "-n", "root",
				"--silent=true", "--showHeader=true", "--outputformat=table"}
			for _, k := range vars {
				args = append(args, "--hivevar", k+"="+job.QueryVariables[k])
			}
			for _, k := range props {
				args = append(args, "--hiveconf", k+"="+job.Properties[k])
			}
			if job.ContinueOnFailure {
				args = append(args, "--force=true")
			}
			// jar_file_uris: "HCFS URIs of jar files to add to the CLASSPATH
			// of the Hive server and Hadoop MapReduce (MR) tasks".
			if len(jars) > 0 {
				init := filepath.Join(dir, "init.sql")
				var b strings.Builder
				for _, j := range jars {
					fmt.Fprintf(&b, "ADD JAR %s;\n", j)
				}
				if err := os.WriteFile(init, []byte(b.String()), 0o644); err != nil {
					return Result{}, err
				}
				args = append(args, "-i", mount+"/init.sql")
			}
			args = append(args, "-f", main)
		}
		run = append(run, "--entrypoint", "/bin/bash", e.Hive, "/cb/eco.sh")
		run = append(run, args...)
	case KindTrino:
		args := []string{"trino", "--server", "http://" + c.nodeAlias(-1) + ":8060", "--file", main, "--user", "root"}
		if job.OutputFormat != "" {
			args = append(args, "--output-format", strings.ToUpper(job.OutputFormat))
		}
		if len(job.ClientTags) > 0 {
			args = append(args, "--client-tags", strings.Join(job.ClientTags, ","))
		}
		for _, k := range props {
			args = append(args, "--session", k+"="+job.Properties[k])
		}
		if job.ContinueOnFailure {
			args = append(args, "--ignore-errors")
		}
		run = append(run, "--entrypoint", args[0], e.Trino)
		run = append(run, args[1:]...)
	case KindFlink:
		args := []string{"run", "-m", c.nodeAlias(-1) + ":8081"}
		if job.MainClass != "" {
			args = append(args, "-c", job.MainClass)
		}
		if job.SavepointURI != "" {
			args = append(args, "-s", job.SavepointURI)
		}
		for _, j := range jars {
			args = append(args, "-C", "file://"+j)
		}
		for _, k := range props {
			args = append(args, "-D"+k+"="+job.Properties[k])
		}
		if main == "" && len(jars) > 0 {
			main = jars[0]
		}
		args = append(args, main)
		args = append(args, job.Args...)
		// Dataproc's paths for the image's files
		// (file:///usr/lib/flink/examples/batch/WordCount.jar).
		script := "mkdir -p /usr/lib && ln -sfn /opt/flink /usr/lib/flink && exec /opt/flink/bin/flink \"$@\""
		run = append(run, "--entrypoint", "/bin/bash", e.Flink, "-c", script, "flink")
		run = append(run, args...)
	default:
		return Result{}, fmt.Errorf("unknown job kind %q", job.Kind)
	}
	return c.runDriver(ctx, job, driver, run)
}

// runDriver runs a job's driver container to its end, its output the
// job's driver output.
func (c *Cluster) runDriver(ctx context.Context, job Job, driver string, run []string) (Result, error) {
	d := c.d
	defer func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", driver) }()
	d.logf("spark: running %s job %s on cluster %s", job.Kind, job.SparkJobID, c.spec.Name)
	cmd := exec.CommandContext(ctx, "docker", run...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return Result{}, &Unavailable{Msg: "start the job's driver: " + err.Error()}
	}
	tail, output := d.follow(job, stdout, nil)
	waitErr := cmd.Wait()
	res := Result{DriverOutput: output}
	if c.yarnEnabled() && (job.Kind == KindHadoop || job.Kind == KindPig) {
		res.YarnApps = c.YarnApps(context.Background(), output)
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return res, &Unavailable{Msg: "the job's driver: " + waitErr.Error()}
		}
		if exit.ExitCode() == 125 {
			return res, &Unavailable{Msg: "the job's driver did not start: " + strings.Join(tail(), "\n")}
		}
		res.Failed = true
		res.Message = ecoFailure(tail())
	}
	return res, nil
}

// ecoFailure is a failed job's error: its last error line, else its last
// line.
func ecoFailure(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "Error:") || strings.Contains(l, "ERROR") || strings.Contains(l, "Exception") || strings.HasPrefix(l, "Query ") && strings.Contains(l, "failed") {
			return l
		}
	}
	if msg := failure(lines); msg != "" {
		return msg
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "the job's driver failed"
}

// fileURIs are paths as a comma-separated list of file: URIs, each
// fragment its base name (the name in a task's working directory).
func fileURIs(paths []string) string {
	var out []string
	for _, p := range paths {
		out = append(out, "file://"+p+"#"+filepath.Base(p))
	}
	return strings.Join(out, ",")
}
