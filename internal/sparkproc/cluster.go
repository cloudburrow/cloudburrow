package sparkproc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Dataproc cluster (#2162) is a Spark standalone cluster in Docker: a
// master node (the standalone Master and the Spark History Server, as a
// Dataproc master runs the History Server) and one container a worker,
// on a network created --internal whose one other peer is the relay of a
// workload's (relay.py): the connector's BigQuery endpoints, and the
// master's web interfaces published on the host's loopback. A job is a
// driver container on that network, spark-submit in client mode against
// the master, its event log in the cluster's history directory, which
// the History Server reads.
//
// Dataproc runs Spark on YARN; a local cluster runs Spark's own
// standalone scheduler, which needs no Hadoop daemons: a job's
// application, its executors on the workers and their logs are Spark's
// either way.

// ClusterSpec is a cluster to start.
type ClusterSpec struct {
	// ID is the cluster's UUID; Name its name, its nodes' host names'
	// prefix (NAME-m, NAME-w-0, ...), as Dataproc names its instances.
	ID, Name string
	// Project and Location are the cluster's.
	Project, Location string
	// ImageVersion is the cluster's image version, MAJOR.MINOR.
	ImageVersion string
	// Workers is the number of worker nodes; 0 is a single node
	// cluster, whose master runs a worker too.
	Workers int
	// WorkerCores and WorkerMemory are each worker's (0 and "": 2 and
	// 2g).
	WorkerCores  int
	WorkerMemory string
	// Properties are the cluster's Spark properties
	// (softwareConfig.properties' spark: ones, unprefixed), every job's
	// defaults.
	Properties map[string]string
	// HistoryOnly starts the History Server alone, on no workers: the
	// instance's own, which serves the Spark UI of batches and sessions.
	HistoryOnly bool
	// HistoryBase is the path the History Server is reached at through
	// the API's proxy (spark.ui.proxyBase).
	HistoryBase string
	// GatewayBase is the cluster's Component Gateway path (ending in
	// /), under which the components' web interfaces are served.
	GatewayBase string
	// Components are the optional components the cluster runs
	// (ComponentTrino, ...).
	Components []string
	// ConfProperties are the cluster's properties of its other engines'
	// files, prefixed as softwareConfig.properties has them (core:,
	// mapred:, hive:, pig:, trino:, flink:).
	ConfProperties map[string]string
	// IcebergVersion is the ICEBERG component's version
	// (dataproc:dataproc.iceberg.version, "" for the image's default).
	IcebergVersion string
	// RangerPassword is the RANGER component's admin password, decrypted
	// from dataproc:ranger.admin.password.uri.
	RangerPassword string
}

// Cluster is a running cluster.
type Cluster struct {
	d       *Docker
	spec    ClusterSpec
	rt      Runtime
	version string
	jar     string

	dir, network, relay, master string
	label                       string
	history                     string // the event logs' host directory
	stopRelay                   func()

	mu      sync.Mutex
	workers []string // worker container names, in order
	extras  []string // the components' containers, in start order
	stopped bool
	gone    bool

	// lakeMounts and lakeProps are the table formats' jars' mounts and
	// Spark properties (setupLake).
	lakeMounts []string
	lakeProps  map[string]string

	// singleNM is set once a single node cluster's NodeManager runs.
	singleNM bool

	hiveMu sync.Mutex
	hiveUp bool

	// groups are the driver node groups' instances by group ID, and
	// groupNodes each instance's container.
	groups     map[string][]string
	groupNodes map[string]string
}

// nodeAlias is a node's host name on the cluster's network.
func (c *Cluster) nodeAlias(i int) string {
	if i < 0 {
		return c.spec.Name + "-m"
	}
	return fmt.Sprintf("%s-w-%d", c.spec.Name, i)
}

// HistoryDir is the host directory of the cluster's event logs, which
// its History Server reads (and the Dataproc API fills from Cloud
// Storage, for a Persistent History Server).
func (c *Cluster) HistoryDir() string {
	if c.history != "" {
		return c.history
	}
	return filepath.Join(c.dir, "history")
}

// Workers is the number of worker containers.
func (c *Cluster) Workers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.workers)
}

// clusterRuntime is the image version's runtime, with the image that
// carries R as well as Python (every cluster image runs SparkR jobs).
func (d *Docker) clusterRuntime(version string) (Runtime, bool) {
	rt, ok := d.Clusters[version]
	if !ok || rt.Connector == nil {
		return Runtime{}, false
	}
	if rt.RImage != "" {
		rt.Image = rt.RImage
	}
	return rt, rt.Image != ""
}

// StartCluster starts spec's nodes and returns once its master answers.
func (d *Docker) StartCluster(ctx context.Context, spec ClusterSpec) (_ *Cluster, err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, &Unavailable{Msg: "docker is not on PATH: Dataproc clusters run in local Spark containers"}
	}
	var rt Runtime
	version := spec.ImageVersion
	if spec.HistoryOnly {
		var ok bool
		rt, version, ok = d.runtime(Job{})
		if !ok {
			return nil, &Unavailable{Msg: "the Spark runner serves no default runtime"}
		}
	} else {
		var ok bool
		if rt, ok = d.clusterRuntime(version); !ok {
			return nil, &Unavailable{Msg: "the Spark runner serves no image version " + version}
		}
	}
	jar, err := rt.Connector(ctx)
	if err != nil {
		return nil, &Unavailable{Msg: "the BigQuery connector for Spark is not available: " + err.Error()}
	}
	rest, storage := d.BigQuery(), d.Storage()
	if rest == "" || storage == "" {
		return nil, &Unavailable{Msg: "the instance's BigQuery REST and Storage API ports are not live"}
	}
	c := &Cluster{d: d, spec: spec, rt: rt, version: version, jar: jar, label: d.instanceLabel()}
	defer func() {
		if err != nil {
			c.Delete()
		}
	}()
	if c.dir, err = os.MkdirTemp(d.Tmp, "cb-dpc-"); err != nil {
		return nil, err
	}
	if spec.HistoryOnly && d.History != "" {
		// The instance's History Server reads the batches' and sessions'
		// event logs.
		c.history = d.History
	}
	for _, name := range []string{"relay.py", "run.sh", "node.sh", "eco.sh", "hivenode.sh", "nodemanager.sh"} {
		b, _ := assets.ReadFile("assets/" + name)
		if err := os.WriteFile(filepath.Join(c.dir, name), b, 0o644); err != nil {
			return nil, err
		}
	}
	if err := writeTLS(filepath.Join(c.dir, "tls")); err != nil {
		return nil, err
	}
	for _, sub := range []string{"history", "jobs", "warehouse", "shared"} {
		if err := os.MkdirAll(filepath.Join(c.dir, sub), 0o777); err != nil {
			return nil, err
		}
		_ = os.Chmod(filepath.Join(c.dir, sub), 0o777)
	}
	_ = filepath.Walk(filepath.Join(c.dir, "tls"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
	_ = os.Chmod(c.dir, 0o755)

	suffix := nameSafe.ReplaceAllString(strings.ToLower(spec.ID), "-")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	base := "cb-dpc-" + strings.Trim(suffix, "-")
	c.network, c.relay, c.master = base+"-net", base+"-relay", base+"-m"

	if err := c.writeConf(filepath.Join(c.dir, "conf"), nil, nil); err != nil {
		return nil, err
	}
	hosts := []string{rest, storage}
	gcs := ""
	if d.GCS != nil {
		gcs = d.GCS()
	}
	if gcs != "" {
		hosts = append(hosts, gcs)
	}
	targets, stop, err := d.hostTargets(ctx, hosts...)
	if err != nil {
		return nil, &Unavailable{Msg: err.Error()}
	}
	restTarget, storageTarget := targets[0], targets[1]
	c.stopRelay = stop
	if out, err := d.docker(ctx, "network", "create", "--internal", "--label", c.label, c.network); err != nil {
		return nil, &Unavailable{Msg: "create the cluster's internal network: " + out}
	}
	alias := c.nodeAlias(-1)
	relayArgs := []string{"run", "-d", "--name", c.relay, "--label", c.label, "-v", c.dir + ":/cb:ro", "--entrypoint", "python3"}
	if !d.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	// The History Server (18080), the master's web UI (18088) and the
	// components' web ports; and the instance's Cloud Storage, for the
	// Hadoop connector, on gcsPort.
	relays := []string{alias + ":18080", alias + ":18088"}
	relayArgs = append(relayArgs, "-p", "127.0.0.1::18080", "-p", "127.0.0.1::18088")
	for _, comp := range spec.Components {
		if p, ok := componentPorts[comp]; ok {
			relayArgs = append(relayArgs, "-p", "127.0.0.1::"+p.listen)
			relays = append(relays, p.listen+"="+alias+":"+p.port)
		}
	}
	if c.yarnEnabled() {
		for _, p := range yarnPorts {
			relayArgs = append(relayArgs, "-p", "127.0.0.1::"+p.port)
			relays = append(relays, alias+":"+p.port)
		}
	}
	if len(targets) > 2 {
		relays = append(relays, gcsPort+"=@"+targets[2])
	}
	relayArgs = append(append(relayArgs, rt.Image, "/cb/relay.py", restTarget, storageTarget), relays...)
	if out, err := d.docker(ctx, relayArgs...); err != nil {
		return nil, &Unavailable{Msg: "start the cluster's relay: " + out}
	}
	if err := d.waitReady(ctx, c.relay); err != nil {
		return nil, &Unavailable{Msg: err.Error()}
	}
	if out, err := d.docker(ctx, "network", "connect", "--alias", restHost, "--alias", storageHost, c.network, c.relay); err != nil {
		return nil, &Unavailable{Msg: "connect the cluster's relay: " + out}
	}
	if err := c.setupLake(ctx); err != nil {
		return nil, err
	}
	role := "master"
	switch {
	case spec.HistoryOnly:
		role = "history"
	case spec.Workers == 0:
		role = "single"
	}
	if err := c.startNode(ctx, c.master, alias, role); err != nil {
		return nil, err
	}
	if err := c.waitLog(ctx, c.master, regexp.MustCompile(`Bound HistoryServer|HistoryServer.*started|Started HistoryServer`), 3*time.Minute); err != nil {
		return nil, err
	}
	if !spec.HistoryOnly {
		if err := c.startDockerd(ctx, c.master); err != nil {
			return nil, err
		}
	}
	if !spec.HistoryOnly {
		if err := c.waitLog(ctx, c.master, regexp.MustCompile(`I have been elected leader|Starting Spark master`), 3*time.Minute); err != nil {
			return nil, err
		}
		if c.yarnEnabled() {
			if err := c.startYARN(ctx); err != nil {
				return nil, err
			}
		}
		if err := c.Scale(ctx, spec.Workers); err != nil {
			return nil, err
		}
		if err := c.startComponents(ctx); err != nil {
			return nil, err
		}
	}
	d.logf("spark: cluster %s (%s) started on image version %s in %s, %d workers", spec.Name, spec.ID, version, rt.Image, spec.Workers)
	return c, nil
}

// startNode runs a node's container, role its node.sh role.
func (c *Cluster) startNode(ctx context.Context, name, alias, role string) error {
	cores, mem := c.spec.WorkerCores, c.spec.WorkerMemory
	if cores <= 0 {
		cores = 2
	}
	if mem == "" {
		mem = "2g"
	}
	args := []string{"run", "-d", "--name", name, "--hostname", alias, "--network-alias", alias, "--label", c.label,
		"--label", "cloudburrow.dataproc.cluster=" + c.spec.ID, "--network", c.network,
		"-v", filepath.Join(c.dir, "tls") + ":/cb/tls:ro", "-v", filepath.Join(c.dir, "node.sh") + ":/cb/node.sh:ro",
		"-v", c.HistoryDir() + ":/cb/history",
		"-e", "PYSPARK_PYTHON=python3", "-e", "SPARK_LOCAL_HOSTNAME=" + alias,
		"-e", fmt.Sprintf("CB_CORES=%d", cores), "-e", "CB_MEMORY=" + mem, "-e", "CB_MASTER=" + c.nodeAlias(-1),
		"-e", "CB_HISTORY_BASE=" + c.spec.HistoryBase}
	args = append(args, c.lakeMounts...)
	args = append(args, c.dockerClient(true)...)
	args = append(args, "--entrypoint", "/bin/bash", c.rt.Image, "/cb/node.sh", role)
	if out, err := c.d.docker(ctx, args...); err != nil {
		return &Unavailable{Msg: "start the cluster's node " + alias + ": " + out}
	}
	return nil
}

// waitLog waits for a line matching re in the container's output.
func (c *Cluster) waitLog(ctx context.Context, name string, re *regexp.Regexp, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		logs, _ := c.d.docker(ctx, "logs", name)
		if re.MatchString(logs) {
			return nil
		}
		if st, _ := c.d.docker(ctx, "inspect", "-f", "{{.State.Running}}", name); st == "false" {
			return fmt.Errorf("the cluster's node %s stopped: %s", name, lastLines(logs, 20))
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the cluster's node %s did not start: %s", name, lastLines(logs, 20))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Scale sets the number of worker nodes to n, adding or removing the
// last ones. A single node cluster (0 workers) cannot be scaled.
func (c *Cluster) Scale(ctx context.Context, n int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return errors.New("the cluster was deleted")
	}
	yarn := c.yarnEnabled()
	var added []string
	for len(c.workers) < n {
		i := len(c.workers)
		name := fmt.Sprintf("%s-w-%d", c.master[:len(c.master)-2], i)
		if err := c.startNode(ctx, name, c.nodeAlias(i), "worker"); err != nil {
			return err
		}
		c.workers = append(c.workers, name)
		if err := c.startDockerd(ctx, name); err != nil {
			return err
		}
		if yarn {
			if err := c.startNodeManager(ctx, name, c.nodeAlias(i)); err != nil {
				return err
			}
			added = append(added, name)
		}
	}
	if yarn && n == 0 && c.spec.Workers == 0 && !c.singleNM {
		// A single node cluster's NodeManager is the master's.
		if err := c.startNodeManager(ctx, c.master, c.nodeAlias(-1)); err != nil {
			return err
		}
		c.singleNM = true
		added = append(added, c.master)
	}
	for len(c.workers) > n {
		last := c.workers[len(c.workers)-1]
		if yarn {
			_, _ = c.d.docker(context.Background(), "rm", "-f", "-v", nodeManager(last))
		}
		if c.dockerImage() != "" {
			_, _ = c.d.docker(context.Background(), "rm", "-f", "-v", dockerd(last))
		}
		_, _ = c.d.docker(context.Background(), "rm", "-f", "-v", last)
		c.workers = c.workers[:len(c.workers)-1]
	}
	for _, w := range c.workers {
		if err := c.waitLog(ctx, w, regexp.MustCompile(`Successfully registered with master`), 3*time.Minute); err != nil {
			return err
		}
	}
	return c.waitNodeManagers(ctx, added)
}

// nodes are every container of the cluster, the relay first.
func (c *Cluster) nodesLocked() []string {
	nodes := append(append(append([]string{c.relay, c.master}, c.workers...), c.extras...), c.groupContainersLocked()...)
	return append(append(nodes, c.nodeManagersLocked()...), c.dockerdsLocked()...)
}

// Stop stops the cluster's containers, keeping them.
func (c *Cluster) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	args := append([]string{"stop", "-t", "10"}, c.nodesLocked()...)
	if out, err := c.d.docker(ctx, args...); err != nil {
		return errors.New("stop the cluster's nodes: " + out)
	}
	c.stopped = true
	return nil
}

// Start starts a stopped cluster's containers again.
func (c *Cluster) Start(ctx context.Context) error {
	c.mu.Lock()
	nodes := c.nodesLocked()
	workers := append([]string(nil), c.workers...)
	c.mu.Unlock()
	args := append([]string{"start"}, nodes...)
	if out, err := c.d.docker(ctx, args...); err != nil {
		return errors.New("start the cluster's nodes: " + out)
	}
	if err := c.d.waitReady(ctx, c.relay); err != nil {
		return err
	}
	if !c.spec.HistoryOnly {
		since := time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
		for _, w := range workers {
			if err := c.waitSince(ctx, w, since, `Successfully registered with master`); err != nil {
				return err
			}
		}
	}
	c.mu.Lock()
	c.stopped = false
	c.mu.Unlock()
	return nil
}

func (c *Cluster) waitSince(ctx context.Context, name, since, pattern string) error {
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		logs, _ := c.d.docker(ctx, "logs", "--since", since, name)
		if re.MatchString(logs) {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the cluster's node %s did not start again: %s", name, lastLines(logs, 20))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Delete removes the cluster's containers, network and directory.
func (c *Cluster) Delete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return
	}
	c.gone = true
	bg := context.Background()
	if c.relay != "" {
		args := append([]string{"rm", "-f", "-v"}, c.nodesLocked()...)
		_, _ = c.d.docker(bg, args...)
		// Drivers still running.
		if out, err := c.d.docker(bg, "ps", "-aq", "--filter", "label=cloudburrow.dataproc.cluster="+c.spec.ID); err == nil {
			for _, id := range strings.Fields(out) {
				_, _ = c.d.docker(bg, "rm", "-f", "-v", id)
			}
		}
	}
	if c.network != "" {
		_, _ = c.d.docker(bg, "network", "rm", c.network)
	}
	if c.stopRelay != nil {
		c.stopRelay()
	}
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
}

// Web is the cluster's web interfaces on the host: "history" the Spark
// History Server's address, "master" the standalone master's UI.
func (c *Cluster) Web(ctx context.Context) map[string]string {
	out := map[string]string{}
	ports := map[string]string{"history": "18080", "master": "18088"}
	if c.yarnEnabled() {
		for _, p := range yarnPorts {
			ports[p.key] = p.port
		}
	}
	for _, comp := range c.spec.Components {
		if p, ok := componentPorts[comp]; ok {
			ports[p.key] = p.listen
		}
	}
	for key, port := range ports {
		if a, err := c.d.docker(ctx, "port", c.relay, port); err == nil {
			out[key] = strings.TrimSpace(strings.Split(a, "\n")[0])
		}
	}
	return out
}

// appIDLine is the line Spark's standalone scheduler logs with the
// application's ID.
var appIDLine = regexp.MustCompile(`Connected to Spark cluster with app ID (\S+)`)

// Submit runs job (a batch-like Kind) on the cluster: its driver in a
// container of its own on the cluster's network, in client mode.
func (c *Cluster) Submit(ctx context.Context, job Job) (Result, error) {
	c.mu.Lock()
	if c.gone || c.stopped {
		c.mu.Unlock()
		return Result{}, errors.New("the cluster is not running")
	}
	c.mu.Unlock()
	switch job.Kind {
	case KindHadoop, KindHive, KindPig, KindTrino, KindFlink:
		return c.submitEco(ctx, job)
	}
	d := c.d
	token := accessToken
	if job.RunAs != "" {
		if d.Mint == nil {
			return Result{}, &Unavailable{Msg: "the Spark runner cannot mint a token for " + job.RunAs}
		}
		tok, err := d.Mint(job.RunAs)
		if err != nil {
			return Result{Failed: true, Message: err.Error()}, nil
		}
		token = tok
	}
	dir, err := os.MkdirTemp(filepath.Join(c.dir, "jobs"), "job-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	args, err := writeJob(dir, job)
	if err != nil {
		return Result{}, err
	}
	if err := writeToken(dir, token); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o755); err != nil {
		return Result{}, err
	}
	ca, err := os.ReadFile(filepath.Join(c.dir, "tls", "ca.pem"))
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "tls", "ca.pem"), ca, 0o644); err != nil {
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
	_ = os.Chmod(filepath.Join(dir, "out"), 0o777)
	suffix := nameSafe.ReplaceAllString(strings.ToLower(job.SparkJobID), "-")
	if len(suffix) > 40 {
		suffix = suffix[len(suffix)-40:]
	}
	driver := c.master[:len(c.master)-2] + "-d-" + strings.Trim(suffix, "-") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
	netArgs, driverHost := c.driverNodeNetwork(job, driver)
	conf := map[string]string{
		"spark.driver.host":                driverHost,
		"spark.submit.deployMode":          "client",
		"spark.eventLog.enabled":           "true",
		"spark.eventLog.dir":               "file:/cb-history",
		"spark.executor.extraJavaOptions":  "-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks -Djavax.net.ssl.trustStorePassword=changeit",
		"spark.executorEnv.PYSPARK_PYTHON": "python3",
	}
	for k, v := range c.lakeProps {
		conf[k] = v
	}
	for k, v := range c.spec.Properties {
		conf[k] = v
	}
	if job.Kind == KindSparkR {
		conf["spark.r.command"] = "Rscript"
	}
	master := "spark://" + c.nodeAlias(-1) + ":7077"
	yarn := c.yarnEnabled()
	if yarn {
		master = "yarn"
		c.yarnSparkConf(job, conf)
	}
	submit := submitArgs(job, args, token, conf)
	submit[1] = master
	run := append(append([]string{"run", "--name", driver, "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID}, netArgs...),
		"-v", dir+":/cb", "-v", c.jar+":/cb-connector/spark-bigquery.jar:ro", "-v", c.HistoryDir()+":/cb-history")
	if yarn {
		run = append(run, "-v", filepath.Join(c.dir, "conf")+":/cb-conf:ro", "-v", filepath.Join(c.dir, "shared")+":/cb-shared",
			"-e", "HADOOP_CONF_DIR=/cb-conf")
	}
	run = append(run, c.lakeMounts...)
	run = append(run, c.dockerClient(false)...)
	run = append(run,
		"-e", "CB_SPARK_OUT=/cb/out/params.json", "-e", "CB_SPARK_TYPES=/cb/types.json", "-e", "PYTHONPATH=/cb/lib", "-e", "PYSPARK_PYTHON=python3",
		"--entrypoint", "/bin/bash", c.rt.Image, "/cb/run.sh")
	run = append(run, submit...)
	defer func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", driver) }()
	d.logf("spark: running %s (%s) on cluster %s", job.Procedure, job.SparkJobID, c.spec.Name)
	cmd := exec.CommandContext(ctx, "docker", run...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return Result{}, &Unavailable{Msg: "start the job's driver: " + err.Error()}
	}
	var appID string
	tail, output := d.follow(job, stdout, func(line string) {
		if m := appIDLine.FindStringSubmatch(line); m != nil && appID == "" {
			appID = m[1]
		}
		if m := yarnAppLine.FindStringSubmatch(line); m != nil && appID == "" {
			appID = m[1]
		}
	})
	waitErr := cmd.Wait()
	res := Result{DriverOutput: output, AppID: appID}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return res, &Unavailable{Msg: "the job's driver: " + waitErr.Error()}
		}
		if exit.ExitCode() == 125 {
			return res, &Unavailable{Msg: "the job's driver did not start: " + strings.Join(tail(), "\n")}
		}
		res.Failed = true
		res.Message = failure(tail())
	}
	return res, nil
}

// Diagnose is the cluster's diagnostic tarball (.tar.gz), laid out as
// Dataproc's: summary.txt at its root, each node's logs under logs/, and
// the cluster's configuration under conf/.
func (c *Cluster) Diagnose(ctx context.Context, summary string) ([]byte, error) {
	c.mu.Lock()
	nodes := append([]string{c.master}, c.workers...)
	c.mu.Unlock()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	var sb strings.Builder
	sb.WriteString(summary)
	fmt.Fprintf(&sb, "\nImage version: %s (%s)\nNodes:\n", c.version, c.rt.Image)
	for i, n := range nodes {
		st, _ := c.d.docker(ctx, "inspect", "-f", "{{.State.Status}} since {{.State.StartedAt}}", n)
		host := c.nodeAlias(i - 1)
		fmt.Fprintf(&sb, "  %s: %s\n", host, st)
	}
	if err := add("summary.txt", []byte(sb.String())); err != nil {
		return nil, err
	}
	for i, n := range nodes {
		host := c.nodeAlias(i - 1)
		logs, _ := c.d.docker(ctx, "logs", "--tail", "5000", n)
		if err := add("logs/"+host+"/spark.log", []byte(logs)); err != nil {
			return nil, err
		}
	}
	var conf strings.Builder
	keys := make([]string, 0, len(c.spec.Properties))
	for k := range c.spec.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&conf, "%s %s\n", k, c.spec.Properties[k])
	}
	if err := add("conf/spark/spark-defaults.conf", []byte(conf.String())); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
