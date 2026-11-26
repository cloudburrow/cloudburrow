// Package beamrun runs Dataflow jobs' Apache Beam pipelines (#2215) on
// Apache Flink's Beam job server, the portable runner Beam publishes for
// Flink (apache/beam_flink1.20_job_server, Apache-2.0), with the job's
// SDK harness (the Beam SDK's own container, apache/beam_python3.12_sdk)
// as an EXTERNAL worker pool beside it. Both are pinned by digest in
// dependencies.json (components.dataflow).
//
// Each job has its own --internal Docker network, whose one route out is
// a relay container that reaches the instance's Cloud Storage, Pub/Sub,
// BigQuery and Dataflow ports on the host and nothing else, as the Spark
// runner's jobs do (internal/sparkproc). On it run:
//
//   - the job server, an embedded Flink cluster ([local]) of the job's
//     parallelism (its worker bound, maxNumWorkers, at most MaxParallelism);
//   - the worker pool, in the job server's network namespace, as Beam's
//     Flink deployments run it (the job server hands its workers
//     localhost endpoints);
//   - the launcher (assets/py/cblaunch.py), in the SDK image, which reads
//     the pipeline the SDK's DataflowRunner staged in Cloud Storage,
//     replaces Dataflow's service-side Pub/Sub transforms with the SDK's
//     own (cbio.py), and submits it through the Beam Job API.
//
// Why Flink (measured 2026-11-24 on this machine, Docker Desktop, 8 CPUs):
// the Beam Python wordcount ran on the Flink 1.20 job server with the
// Python 3.12 worker pool in 15 s end to end, batch and streaming
// (unbounded splittable DoFns with checkpointing) are both supported by
// Beam's Flink runner, and it is one container; Spark's portable runner
// runs batch only for Python (Beam's runner capability matrix), so a
// streaming job with drain could not run on it.
package beamrun

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

//go:embed assets/relay.py assets/py/cblaunch.py assets/py/cbio.py assets/py/sitecustomize.py assets/py/cbfnproxy.py assets/py/cbnet.py assets/java/pool.sh assets/java/CbCredentialFactory.java assets/java/GreedyPCollectionFusers.java assets/java/FlinkBatchPortablePipelineTranslator.java assets/java/ExecutableStageDoFnOperator.java
var assets embed.FS

// The pinned images (dependencies.json components.dataflow).
const (
	// jobServerScript starts the job server (JobServerImage's
	// flink-job-server.sh) with the patched classes before its jar, and
	// with 70% of its container's memory as heap rather than the JVM's
	// default quarter: Flink's local cluster clones the job graph as it
	// submits it, and each fused stage of a large pipeline carries the
	// pipeline's components (Text Files on Cloud Storage to Cloud
	// Spanner's 933 steps came to about 200 MB of them, and its job failed
	// with "Java heap space" in about half its runs).
	jobServerScript = `javac -nowarn -d /tmp/cb-patch -cp "jars/*" /cb-patch/*.java >/tmp/cb-patch.log 2>&1 || { cat /tmp/cb-patch.log; exit 1; }
exec java -XX:MaxRAMPercentage=70 -cp "/tmp/cb-patch:jars/*" org.apache.beam.runners.flink.FlinkJobServerDriver "$@"`
	// JobServerImage is Beam 2.78.0's Flink 1.20 job server.
	JobServerImage = "apache/beam_flink1.20_job_server@sha256:b1f7e43440a148151e9db001dfc0498eb4f87bdd4d6808904daf52ab0b532053"
	// BeamVersion is the Beam release whose SDK harnesses are pinned.
	BeamVersion = "2.78.0"
)

// PythonImages are Beam 2.78.0's Python SDK containers, by Python
// version.
var PythonImages = map[string]string{
	"3.12": "apache/beam_python3.12_sdk@sha256:bf536d721f825259329a396a20947b81ce8d0bbe08472b26f3f149c9957c1e56",
}

// JavaImages are Beam 2.78.0's Java SDK containers, by Java major
// version. A Java job's workers run in the one its pipeline's environment
// names (beam_javaNN_sdk), as Beam's Java worker pool
// (org.apache.beam.fn.harness.ExternalWorkerService) on the job's staged
// jars; its Pub/Sub reads and writes, which Dataflow's service runs itself,
// run in the Python SDK's harness beside it.
var JavaImages = map[string]string{
	"11": "apache/beam_java11_sdk@sha256:20c289f6288972c9bbbe63effdda9d8b1f207b5597c96bb42c83012cb5e7f532",
	"17": "apache/beam_java17_sdk@sha256:8acfe1acfd173b80474c12833397ca1d19b432a3aa5d0d160c5f8fd4abf95126",
	"21": "apache/beam_java21_sdk@sha256:96816b03bebe628d9ffd2bd47f37167e055fae2ab99890ad6e941aa908440740",
}

// GoImage is Beam 2.78.0's Go SDK container. A Go job's workers are the
// pipeline's own binary (the SDK's staged worker), which the container's
// boot fetches from the job server and runs; the boot runs as the
// container executable of a Beam worker pool (worker_pool_main), on
// goPoolPort, as the Go container's own pool takes the Python pool's
// fixed port. Its Pub/Sub reads and writes run in Python's, as Java's.
const GoImage = "apache/beam_go_sdk@sha256:6290be71b91b975153ed86ffe7e05f3d36376c3e46ece68ea08de366fba17fab"

// LauncherImage is the Python SDK container the launcher, the relay and
// the Python worker pool of a job of another SDK run in.
const LauncherImage = "3.12"

// MaxParallelism bounds a job's Flink parallelism, whatever its
// maxNumWorkers: one local machine runs every job.
const MaxParallelism = 4

// InstanceLabel labels every container and network made, with the
// instance's name.
const InstanceLabel = "cloudburrow.dev/dataflow-instance"

// The relay's ports on the job's network, and its name there: under
// .localhost, as the instance's host check (internal/hostguard, ADR-0004)
// answers such names, and with "localhost" in it, as the Python SDK's
// DataflowRunner speaks REST (not TLS gRPC) to an endpoint whose host
// has; Dataflow's on port 80, as the SDK takes an endpoint without a port.
const (
	relayName    = "relay.cloudburrow.localhost"
	storagePort  = 9000
	pubsubPort   = 9001
	bigqueryPort = 9002
	// The database emulators Google's Datastore, Bigtable and Spanner
	// connectors reach through their emulator variables.
	datastorePort = 9003
	bigtablePort  = 9004
	spannerPort   = 9005
	// registryPort is the instance's Artifact Registry (its package
	// formats' protocols), routed for Flex Template launchers only: a
	// Python template's packages are installed from its Python
	// repositories.
	registryPort = 9006
	// metadataPort is the instance's metadata server, routed for a
	// provided Flex Template's launcher only (launcherMetadata): Google's
	// launcher VM reads its credentials from its metadata server, and so
	// does a template's own client built with application default
	// credentials (BigQuery to Parquet's Storage API client).
	metadataPort = 9007
	// googleTLSPort answers the TLS of a Google API host a job's clients
	// dial at its default endpoint (googleHosts), which the job's
	// containers resolve to the relay: by the name the client asks for.
	googleTLSPort  = 443
	dataflowPort   = 80
	workerPoolPort = 50000
	// The Java worker pool the pipeline's environment names
	// (cblaunch.py's --java-harness), cbfnproxy's, and the Java SDK's own
	// pool behind it.
	javaPoolPort    = 50001
	javaSDKPoolPort = 50002
	// The Go worker pool the pipeline's environment names (cblaunch.py's
	// --go-harness).
	goPoolPort = 50003
	// privateBasePort is the relay's port for the job network's first
	// private services address (PrivateService), the next one's the next
	// port, and so on.
	privateBasePort = 9100
)

// PrivateService is an address on a VPC network's private services
// access range that a Dataflow worker on that network reaches, as a
// Compute Engine VM there does: a Cloud SQL instance's private IP and
// port, and the host address that serves it. The JDBC templates' guides
// require that "the relational database must be accessible from the
// subnet where Dataflow runs" (templates/provided/jdbc-to-bigquery, read
// 2026-11-25), and the workers' network is their network or subnetwork
// option, else "an auto mode VPC network named default"
// (guides/specifying-networks, read 2026-11-25).
type PrivateService struct {
	// Name says what it is ("cloudsql INSTANCE").
	Name string
	// IP and Port are the address the workers dial.
	IP   string
	Port int
	// Target is the host address (host:port) it is served at.
	Target string
}

// Job is one job to run.
type Job struct {
	Project, JobID, Name string
	Streaming            bool
	// PipelineURL is the staged pipeline; Options the SDK's pipeline
	// options.
	PipelineURL string
	Options     map[string]any
	// SDK and SDKVersion are the submitting SDK's language and Beam
	// version; PythonVersion its Python's (MAJOR.MINOR).
	SDK, SDKVersion, PythonVersion string
	// Workers is the job's worker bound (maxNumWorkers).
	Workers int
	// Drain is closed to drain the job.
	Drain <-chan struct{}
	// Snapshot is the snapshot the job's state is restored from ("" none).
	Snapshot string
	// Network and Subnetwork are the VPC network the job's workers run on
	// (its network and subnetwork options, workerPools[].network and
	// .subnetwork), and Region its regional endpoint: Dataflow's default
	// network, "default", when neither is set.
	Network, Subnetwork, Region string
}

// Events receives what a job does.
type Events struct {
	Started func(workers int)
	Message func(importance, text string)
	Graph   func(ts []Transform)
	Metrics func(ms []Metric)
	Log     func(severity, text string)
}

// Transform is one transform of the pipeline, as the launcher reads it.
type Transform struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Parent  string   `json:"parent"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Stage   string   `json:"stage"`
}

// Metric is one metric the job server reports.
type Metric struct {
	Name        string `json:"name"`
	Origin      string `json:"origin"`
	Namespace   string `json:"namespace"`
	Step        string `json:"step"`
	PCollection string `json:"pcollection"`
	Kind        string `json:"kind"`
	Value       int64  `json:"value"`
	Count       int64  `json:"count"`
	Total       int64  `json:"total"`
	Min         int64  `json:"min"`
	Max         int64  `json:"max"`
	// Tentative is an attempted value (every bundle attempted), not a
	// committed one (the bundles that finished successfully).
	Tentative bool `json:"tentative"`
}

// Result is how a job ended: State DONE, FAILED or CANCELLED.
type Result struct {
	State, Message string
	// tail is the launcher's last lines that were not events.
	tail []string
}

// Docker runs jobs in the host's Docker.
type Docker struct {
	// Instance labels what is made.
	Instance string
	// Live is the instance's bound addresses on the host (storage,
	// pubsub, bigquery, dataflow), read at each run.
	Live func() map[string]string
	// Tmp holds each job's directory ("": the system's).
	Tmp string
	// Token is the access token the job's clients send.
	Token func() string
	// Registry is the instance's Artifact Registry address on the host
	// (host:port), which a Flex Template's image is read from; nil or ""
	// for none. RegistryToken is the token it is read with.
	Registry, RegistryToken func() string
	Logf                    func(format string, args ...any)
	// Snapshots is the directory the jobs' snapshots (Flink savepoints)
	// are kept in, one directory each ("": none are taken).
	Snapshots string
	// Private answers the private services addresses on a job's VPC
	// network (its project, and its network and subnetwork options as the
	// job gives them, in region), which the job's workers reach and
	// nothing else of the network; nil for none.
	Private func(project, network, subnetwork, region string) []PrivateService

	// closing is set once the instance stops (Shutdown): a cancelled
	// run's containers are then removed at once, not cancelled through
	// the runner.
	closing atomic.Bool

	runMu   sync.Mutex
	running map[string]string // job ID: its directory

	probeMu sync.Mutex
	probed  bool
	desktop bool
}

func (d *Docker) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d *Docker) docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var nameSafe = regexp.MustCompile(`[^a-z0-9-]+`)

func failed(msg string) Result {
	return Result{State: "FAILED", Message: "Workflow failed. Causes: " + msg}
}

// Run runs job to its end.
func (d *Docker) Run(ctx context.Context, job Job, ev Events) Result {
	if _, err := exec.LookPath("docker"); err != nil {
		return failed("docker is not on PATH: Dataflow jobs run on a local Apache Flink runner in Docker.")
	}
	var image string
	switch job.SDK {
	case "Python":
		image = PythonImages[job.PythonVersion]
		if image == "" || job.SDKVersion != BeamVersion {
			return failed(fmt.Sprintf("The job was submitted by Apache Beam %s on Python %s: CloudBurrow's local runner has the SDK harness of Apache Beam %s on Python %s only.", job.SDKVersion, job.PythonVersion, BeamVersion, strings.Join(pythonVersions(), ", ")))
		}
	case "Java":
		if job.SDKVersion != BeamVersion {
			return failed(fmt.Sprintf("The job was submitted by the Apache Beam SDK for Java %s: CloudBurrow's local runner has the SDK harness of Apache Beam %s.", job.SDKVersion, BeamVersion))
		}
		image = PythonImages[LauncherImage]
	case "Go":
		if job.SDKVersion != BeamVersion {
			return failed(fmt.Sprintf("The job was submitted by the Apache Beam SDK for Go %s: CloudBurrow's local runner has the SDK harness of Apache Beam %s.", job.SDKVersion, BeamVersion))
		}
		image = PythonImages[LauncherImage]
	default:
		return failed(fmt.Sprintf("The job was submitted by the Apache Beam %s SDK: CloudBurrow's local runner serves the Python, Java and Go SDKs' pipelines (Apache Beam %s).", job.SDK, BeamVersion))
	}
	live := map[string]string{}
	if d.Live != nil {
		live = d.Live()
	}
	if live["storage"] == "" {
		return failed("The job's staged pipeline is in Cloud Storage, which this instance does not serve.")
	}
	var cleanup []func()
	defer func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	dir, err := os.MkdirTemp(d.Tmp, "cb-dataflow-")
	if err != nil {
		return failed(err.Error())
	}
	cleanup = append(cleanup, func() { _ = os.RemoveAll(dir) })
	if err := writeAssets(dir); err != nil {
		return failed(err.Error())
	}
	opts, _ := json.Marshal(job.Options)
	if err := os.WriteFile(filepath.Join(dir, "options.json"), opts, 0o644); err != nil {
		return failed(err.Error())
	}
	for _, sub := range []string{"ctl", "metrics", "java", "go", "flink"} {
		_ = os.Mkdir(filepath.Join(dir, sub), 0o777)
		_ = os.Chmod(filepath.Join(dir, sub), 0o777)
	}
	// The embedded Flink cluster's REST API on a fixed port of the job's
	// network, which the launcher takes savepoints through (snapshots).
	var savepoint string
	if job.Snapshot != "" {
		loc, err := d.snapshotLocation(job.Snapshot)
		if err != nil {
			return failed(err.Error())
		}
		savepoint = loc
	}
	conf := flinkConfig
	if savepoint != "" {
		// Beam's Flink runner applies its savepoint_path option to a
		// remote cluster only: the embedded one ([local]) is restored
		// through Flink's own configuration.
		conf += "execution.savepoint.path: " + savepoint + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "flink", "config.yaml"), []byte(conf), 0o644); err != nil {
		return failed(err.Error())
	}
	d.runMu.Lock()
	if d.running == nil {
		d.running = map[string]string{}
	}
	d.running[job.JobID] = dir
	d.runMu.Unlock()
	cleanup = append(cleanup, func() {
		d.runMu.Lock()
		delete(d.running, job.JobID)
		d.runMu.Unlock()
	})

	suffix := nameSafe.ReplaceAllString(strings.ToLower(job.JobID), "-")
	if len(suffix) > 30 {
		suffix = suffix[len(suffix)-30:]
	}
	suffix = strings.Trim(suffix, "-")
	network, relay, js, pool, launch := "cb-df-net-"+suffix, "cb-df-relay-"+suffix, "cb-df-flink-"+suffix, "cb-df-sdk-"+suffix, "cb-df-launch-"+suffix
	label := InstanceLabel + "=" + d.Instance
	bg := func(args ...string) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, args...)
	}

	var private []PrivateService
	if d.Private != nil {
		private = d.Private(job.Project, job.Network, job.Subnetwork, job.Region)
	}
	for _, p := range private {
		d.logf("dataflow: job %s reaches %s at %s:%d", job.JobID, p.Name, p.IP, p.Port)
	}
	relayIP, stopNet, err := d.jobNetwork(ctx, network, relay, dir, image, label, live, private...)
	cleanup = append(cleanup, stopNet)
	if err != nil {
		return failed(err.Error())
	}

	parallelism := job.Workers
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > MaxParallelism {
		parallelism = MaxParallelism
	}
	env := d.clientEnv(job, dir, relayIP)
	// The job server's JVM gets the emulator variables too: Java I/O whose
	// splitting runs there (a Google-provided template's SpannerIO read
	// partitions its query when the job starts) reaches the instance's
	// emulators, as the workers do, and never the default endpoints.
	// Its workers share its network namespace and hosts file, so they
	// resolve the googleHosts to the relay too.
	jsArgs := append(append([]string{"run", "-d", "--name", js, "--label", label, "--network", network, "--memory", "3g",
		"-v", filepath.Join(dir, "flink") + ":/cb-flink:ro"}, googleHostArgs(live, relayIP)...), env...)
	if d.Snapshots != "" {
		if err := os.MkdirAll(d.Snapshots, 0o777); err != nil {
			return failed(err.Error())
		}
		jsArgs = append(jsArgs, "-v", d.Snapshots+":"+snapshotMount)
	}
	// The job server runs with three of Beam's classes patched
	// (third_party/apache-beam/PROVENANCE.md): the fuser
	// (GreedyPCollectionFusers.java, apache/beam#34647), without which a
	// Flatten one of whose inputs is also consumed elsewhere (a Spanner
	// export's "Combine all files") fails the job's translation ("were
	// consumed but never produced"), and the batch translator
	// (FlinkBatchPortablePipelineTranslator.java), whose Flatten took only
	// inputs of one coder, and the stateful stage's operator
	// (ExecutableStageDoFnOperator.java), which never ran a DoFn's
	// @OnWindowExpiration (GroupIntoBatches' last batch, so an auto-sharded
	// file write's last files, were dropped at its window's end). They are
	// compiled against the job server's jar and put before it on the
	// classpath (as flink-job-server.sh runs it).
	for _, f := range []string{"GreedyPCollectionFusers.java", "FlinkBatchPortablePipelineTranslator.java", "ExecutableStageDoFnOperator.java"} {
		jsArgs = append(jsArgs, "-v", filepath.Join(dir, "java", f)+":/cb-patch/"+f+":ro")
	}
	jsArgs = append(jsArgs, "--entrypoint", "sh", JobServerImage, "-c", jobServerScript, "sh",
		"--flink-master=[local]", "--job-host=0.0.0.0", "--flink-conf-dir=/cb-flink")
	if out, err := d.docker(ctx, jsArgs...); err != nil {
		return failed("start the Flink job server: " + out)
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", js) })
	if len(private) > 0 {
		psa := "cb-df-psa-" + suffix
		cleanup = append(cleanup, func() { bg("rm", "-f", "-v", psa) })
		if err := d.privateServices(ctx, js, psa, dir, image, label, relayIP, private); err != nil {
			return failed(err.Error())
		}
	}
	// The workers write their metrics' totals to metrics/ (sitecustomize).
	poolArgs := append([]string{"run", "-d", "--name", pool, "--label", label, "--network", "container:" + js, "-v", dir + ":/cb:ro",
		"-v", filepath.Join(dir, "metrics") + ":/cb/metrics", "-e", "CB_METRICS_DIR=/cb/metrics"}, env...)
	poolArgs = append(poolArgs, image, "--worker_pool")
	if out, err := d.docker(ctx, poolArgs...); err != nil {
		return failed("start the job's SDK harness: " + out)
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", pool) })

	// The SDK harness's lines are the job's worker logs.
	logCtx, stopLogs := context.WithCancel(context.Background())
	cleanup = append(cleanup, stopLogs)
	go d.follow(logCtx, pool, ev.Log)
	// A Java pipeline's, and a Python pipeline's cross-language
	// transforms' (Java environments the expansion service added): the
	// Java worker pool starts once the launcher finds a Java environment
	// in the pipeline (java/ready), whichever SDK submitted it.
	jpool := "cb-df-java-" + suffix
	cleanup = append(cleanup, func() {
		if _, err := os.Stat(filepath.Join(dir, "java", "ready")); err == nil {
			bg("rm", "-f", "-v", jpool, jpool+"-fn")
		}
	})
	go d.javaPool(logCtx, dir, jpool, js, label, env, ev.Log)
	// A Go pipeline's: once the launcher finds a Go environment (go/ready).
	gpool := "cb-df-go-" + suffix
	cleanup = append(cleanup, func() {
		if _, err := os.Stat(filepath.Join(dir, "go", "ready")); err == nil {
			bg("rm", "-f", "-v", gpool, gpool+"-boot")
		}
	})
	// The Go SDK's Cloud Storage client reads objects through the XML
	// API at STORAGE_EMULATOR_HOST's root, where the instance takes a
	// .localhost host's first label for a virtual-hosted bucket: the Go
	// workers name the relay by its address.
	goEnv := append(append([]string{}, env...), "-e", fmt.Sprintf("STORAGE_EMULATOR_HOST=http://%s:%d", relayIP, storagePort))
	go d.goPool(logCtx, dir, gpool, js, label, image, goEnv, ev.Log)

	runArgs := append([]string{"run", "--rm", "--name", launch, "--label", label, "--network", network, "-v", dir + ":/cb"}, env...)
	runArgs = append(runArgs, "--entrypoint", "python", image, "/cb/py/cblaunch.py",
		"--job-endpoint", js+":8099", "--artifact-endpoint", js+":8098",
		"--pipeline-url", job.PipelineURL, "--options", "/cb/options.json", "--job-name", job.Name,
		"--parallelism", fmt.Sprint(parallelism), "--cancel-file", "/cb/ctl/cancel",
		"--flink-rest", js+":"+fmt.Sprint(flinkRESTPort))
	if savepoint != "" {
		runArgs = append(runArgs, "--savepoint-path", savepoint)
	}
	if job.Streaming {
		runArgs = append(runArgs, "--streaming")
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", launch) })
	cmd := exec.Command("docker", runArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed(err.Error())
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return failed("start the job's launcher: " + err.Error())
	}
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if d.closing.Load() {
				// The instance is stopping: its containers go at once, as
				// a stopped instance's workers would, so that the stop is
				// not held up by the runner's cancel.
				c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, _ = d.docker(c, "rm", "-f", "-v", launch, js, pool, "cb-df-psa-"+suffix, jpool, jpool+"-fn", gpool, gpool+"-boot", relay)
				return
			}
			// Cancelled: the launcher cancels the Flink job.
			_ = os.WriteFile(filepath.Join(dir, "ctl", "cancel"), nil, 0o644)
		case <-waitDone:
		}
	}()
	go func() {
		if job.Drain == nil {
			return
		}
		select {
		case <-job.Drain:
			_ = os.WriteFile(filepath.Join(dir, "ctl", "drain"), nil, 0o644)
		case <-ctx.Done():
		}
	}()
	res := d.read(stdout, ev, parallelism)
	waitErr := cmd.Wait()
	close(waitDone)
	if res.State == "" {
		if ctx.Err() != nil {
			return Result{State: "CANCELLED"}
		}
		msg := "the job's launcher ended without a result"
		if waitErr != nil {
			msg += ": " + waitErr.Error()
		}
		// Its last lines (a Python traceback's last is its error).
		if len(res.tail) > 0 {
			msg += ": " + strings.Join(res.tail, " | ")
		}
		return failed(msg)
	}
	return res
}

// javaPool starts the job's Java worker pool, in the job server's network
// namespace, once the launcher has copied the job's jars and written its
// options and Java version (java/ready); the launcher waits for it.
func (d *Docker) javaPool(ctx context.Context, dir, name, js, label string, env []string, log func(severity, text string)) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "java", "ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	v, _ := os.ReadFile(filepath.Join(dir, "java", "version"))
	image := JavaImages[strings.TrimSpace(string(v))]
	if image == "" {
		if log != nil {
			log("ERROR", fmt.Sprintf("The pipeline runs on Java %s: CloudBurrow's local runner has the Apache Beam %s SDK harness for Java %s.", strings.TrimSpace(string(v)), BeamVersion, strings.Join(javaVersions(), ", ")))
		}
		return
	}
	// The pool the runner starts workers through is cbfnproxy's, which
	// reads their metrics; it starts each on the Java SDK's own pool.
	proxy := name + "-fn"
	pargs := []string{"run", "-d", "--name", proxy, "--label", label, "--network", "container:" + js, "-v", dir + ":/cb:ro",
		"-v", filepath.Join(dir, "metrics") + ":/cb/metrics", "-e", "PYTHONPATH=/cb/py", "--entrypoint", "python",
		PythonImages[LauncherImage], "/cb/py/cbfnproxy.py", "--listen", fmt.Sprintf("0.0.0.0:%d", javaPoolPort),
		"--target", fmt.Sprintf("localhost:%d", javaSDKPoolPort), "--metrics", "/cb/metrics"}
	if out, err := d.docker(ctx, pargs...); err != nil {
		if log != nil {
			log("ERROR", "start the job's Java worker pool: "+out)
		}
		return
	}
	go d.follow(ctx, proxy, log)
	args := append([]string{"run", "-d", "--name", name, "--label", label, "--network", "container:" + js, "--memory", "3g", "-v", dir + ":/cb:ro"}, env...)
	args = append(args, "--entrypoint", "sh", image, "/cb/java/pool.sh")
	if out, err := d.docker(ctx, args...); err != nil {
		if log != nil {
			log("ERROR", "start the job's Java SDK harness: "+out)
		}
		return
	}
	d.follow(ctx, name, log)
}

// goPool starts the job's Go worker pool, in the job server's network
// namespace, once the launcher has found a Go environment (go/ready): Beam's
// worker pool (worker_pool_main, in the Python SDK's container) on
// goPoolPort, which starts each worker with the Go SDK container's boot
// (copied to go/boot), as the Go container's own pool would.
func (d *Docker) goPool(ctx context.Context, dir, name, js, label, image string, env []string, log func(severity, text string)) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go", "ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	fail := func(what, out string) {
		if log != nil {
			log("ERROR", what+": "+out)
		}
	}
	boot := name + "-boot"
	if out, err := d.docker(ctx, "create", "--name", boot, "--label", label, GoImage); err != nil {
		fail("fetch the Go SDK harness", out)
		return
	}
	out, err := d.docker(ctx, "cp", boot+":/opt/apache/beam/boot", filepath.Join(dir, "go", "boot"))
	_, _ = d.docker(context.Background(), "rm", "-f", "-v", boot)
	if err != nil {
		fail("fetch the Go SDK harness's boot", out)
		return
	}
	args := append([]string{"run", "-d", "--name", name, "--label", label, "--network", "container:" + js, "--memory", "3g",
		"-v", dir + ":/cb:ro"}, env...)
	args = append(args, "--entrypoint", "python", image, "-m", "apache_beam.runners.worker.worker_pool_main",
		fmt.Sprintf("--service_port=%d", goPoolPort), "--container_executable=/cb/go/boot")
	if out, err := d.docker(ctx, args...); err != nil {
		fail("start the job's Go SDK harness", out)
		return
	}
	d.follow(ctx, name, log)
}

func javaVersions() []string {
	var out []string
	for v := range JavaImages {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func pythonVersions() []string {
	var out []string
	for v := range PythonImages {
		out = append(out, v)
	}
	return out
}

// clientEnv is the job's clients' environment on its network.
// jobNetwork makes network, an --internal Docker network whose one other
// peer is relay (named relayName on it), which forwards the instance's
// Cloud Storage, Pub/Sub, BigQuery and Dataflow ports to the host; it
// answers the relay's address on the network. stop undoes what was made,
// also on an error.
//
// Each private service is given a port of the relay (privateBasePort on),
// which relays it to its target; privateServices puts its address in the
// workers' namespace.
func (d *Docker) jobNetwork(ctx context.Context, network, relay, dir, image, label string, live map[string]string, private ...PrivateService) (relayIP string, stop func(), err error) {
	var cleanup []func()
	stop = func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}
	bg := func(args ...string) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = d.docker(c, args...)
	}
	var routes, targets []string
	ports := []int{}
	for _, r := range []struct {
		name string
		port int
	}{{"storage", storagePort}, {"pubsub", pubsubPort}, {"bigquery", bigqueryPort}, {"dataflow", dataflowPort},
		{"datastore", datastorePort}, {"bigtable", bigtablePort}, {"spanner", spannerPort}, {"registry", registryPort}} {
		if a := live[r.name]; a != "" {
			targets = append(targets, a)
			ports = append(ports, r.port)
		}
	}
	// The Storage Read and Write APIs (gRPC) on BigQuery's own port: the
	// Java SDK's BigQueryIO roots its REST client and its Storage API
	// clients at the one bigQueryEndpoint option, and its gRPC clients
	// speak TLS, so the relay answers a TLS connection on that port with
	// the instance's Storage API port (a certificate of relayName, from a
	// CA made for the job, which the Java workers trust) and any other
	// with its REST port.
	//
	// Spanner's port answers TLS too, with the instance's Spanner: the
	// Java SDK's SpannerIO (and DataflowTemplates' copy of it) is given a
	// host (spannerHost, default https://batch-spanner.googleapis.com),
	// and the Java client's SpannerOptions.Builder.setHost clears
	// SPANNER_EMULATOR_HOST (java-spanner 6.113.0), so its channel is
	// TLS; the Python and Go clients' plain connections still reach it.
	tlsPorts := map[int]bool{}
	google := jobGoogleHosts(live)
	if (live["bigquery"] != "" && live["bigquery-storage"] != "") || live["spanner"] != "" || len(google) > 0 {
		if err := writeRelayTLS(filepath.Join(dir, "tls"), google...); err != nil {
			return "", stop, err
		}
	}
	if live["bigquery"] != "" && live["bigquery-storage"] != "" {
		tlsPorts[len(targets)] = true
		targets = append(targets, live["bigquery-storage"])
		ports = append(ports, bigqueryPort)
	}
	if live["spanner"] != "" {
		tlsPorts[len(targets)] = true
		targets = append(targets, live["spanner"])
		ports = append(ports, spannerPort)
	}
	if a := live[launcherMetadata]; a != "" {
		targets = append(targets, a)
		ports = append(ports, metadataPort)
	}
	sni := map[int]string{}
	for _, h := range google {
		tlsPorts[len(targets)] = true
		sni[len(targets)] = h
		targets = append(targets, live[googleHosts[h]])
		ports = append(ports, googleTLSPort)
	}
	for i, p := range private {
		targets = append(targets, p.Target)
		ports = append(ports, privateBasePort+i)
	}
	host, stopHost, err := d.hostTargets(ctx, targets...)
	if err != nil {
		return "", stop, err
	}
	cleanup = append(cleanup, stopHost)
	for i, p := range ports {
		if n := sni[i]; n != "" {
			routes = append(routes, fmt.Sprintf("%d+tls@%s=%s", p, n, host[i]))
			continue
		}
		if tlsPorts[i] {
			routes = append(routes, fmt.Sprintf("%d+tls=%s", p, host[i]))
			continue
		}
		routes = append(routes, fmt.Sprintf("%d=%s", p, host[i]))
	}
	if out, err := d.docker(ctx, "network", "create", "--internal", "--label", label, network); err != nil {
		return "", stop, errors.New("create the job's internal network: " + out)
	}
	cleanup = append(cleanup, func() { bg("network", "rm", network) })
	relayArgs := []string{"run", "-d", "--name", relay, "--label", label, "-v", dir + ":/cb:ro", "--entrypoint", "python3"}
	if !d.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	relayArgs = append(relayArgs, image, "/cb/relay.py")
	relayArgs = append(relayArgs, routes...)
	if out, err := d.docker(ctx, relayArgs...); err != nil {
		return "", stop, errors.New("start the job's relay: " + out)
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", relay) })
	if err := d.waitLog(ctx, relay, "ready", 2*time.Minute); err != nil {
		return "", stop, err
	}
	if out, err := d.docker(ctx, "network", "connect", "--alias", relayName, network, relay); err != nil {
		return "", stop, errors.New("connect the job's relay: " + out)
	}
	// gRPC clients resolve any name under .localhost to loopback (RFC
	// 6761), so Pub/Sub's is given the relay's address on the network.
	ip, err := d.docker(ctx, "inspect", "-f", "{{(index .NetworkSettings.Networks \""+network+"\").IPAddress}}", relay)
	ip = strings.TrimSpace(ip)
	if err != nil || net.ParseIP(ip) == nil {
		return "", stop, errors.New("find the job's relay's address: " + ip)
	}
	return ip, stop, nil
}

// privateServices makes the job's private services addresses reachable
// in the network namespace of js, which the workers share: cbnet.py, run
// once with CAP_NET_ADMIN in that namespace, puts each address on its
// loopback, and psa, the job's private services relay (relay.py, with no
// added capability), answers each address's port there and relays it to
// the job's relay, whose port for it reaches its target on the host. The
// workers' namespace has no other route to the network's addresses (the
// job's network is --internal), as Dataflow's workers reach only what
// their VPC network routes.
func (d *Docker) privateServices(ctx context.Context, js, psa, dir, image, label, relayIP string, private []PrivateService) error {
	ips := []string{}
	seen := map[string]bool{}
	var routes []string
	for i, p := range private {
		if net.ParseIP(p.IP).To4() == nil || p.Port <= 0 {
			return fmt.Errorf("the private services address %s:%d of %s is not an IPv4 address and port", p.IP, p.Port, p.Name)
		}
		if !seen[p.IP] {
			seen[p.IP] = true
			ips = append(ips, p.IP)
		}
		routes = append(routes, fmt.Sprintf("%s:%d=%s:%d", p.IP, p.Port, relayIP, privateBasePort+i))
	}
	args := append([]string{"run", "--rm", "--label", label, "--network", "container:" + js, "--cap-add", "NET_ADMIN",
		"-v", dir + ":/cb:ro", "--entrypoint", "python", image, "/cb/py/cbnet.py"}, ips...)
	if out, err := d.docker(ctx, args...); err != nil {
		return errors.New("put the job's private services addresses on its workers' network: " + out)
	}
	args = append([]string{"run", "-d", "--name", psa, "--label", label, "--network", "container:" + js,
		"-v", dir + ":/cb:ro", "--entrypoint", "python3", image, "/cb/relay.py"}, routes...)
	if out, err := d.docker(ctx, args...); err != nil {
		return errors.New("start the job's private services relay: " + out)
	}
	return d.waitLog(ctx, psa, "ready", 2*time.Minute)
}

func (d *Docker) clientEnv(job Job, dir, relayIP string) []string {
	token := "cloudburrow-dataflow"
	if d.Token != nil {
		if t := d.Token(); t != "" {
			token = t
		}
	}
	host := relayName
	return []string{
		"-e", "CB_DATAFLOW=1", "-e", "CB_PROJECT=" + job.Project, "-e", "CB_ACCESS_TOKEN=" + token,
		"-e", "PYTHONPATH=/cb/py", "-e", "CB_DRAIN=/cb/ctl/drain",
		"-e", fmt.Sprintf("STORAGE_EMULATOR_HOST=http://%s:%d", host, storagePort),
		"-e", fmt.Sprintf("PUBSUB_EMULATOR_HOST=%s:%d", relayIP, pubsubPort),
		"-e", fmt.Sprintf("DATASTORE_EMULATOR_HOST=%s:%d", relayIP, datastorePort),
		"-e", fmt.Sprintf("BIGTABLE_EMULATOR_HOST=%s:%d", relayIP, bigtablePort),
		"-e", fmt.Sprintf("SPANNER_EMULATOR_HOST=%s:%d", relayIP, spannerPort),
		// The Java Spanner client given a host (SpannerIO's spannerHost)
		// is not in emulator mode, and so exports built-in metrics to Cloud
		// Monitoring unless this is set; the job's network has no route
		// there, and no client should try.
		"-e", "SPANNER_DISABLE_BUILTIN_METRICS=true",
		"-e", fmt.Sprintf("BIGQUERY_EMULATOR_HOST=http://%s:%d", host, bigqueryPort),
		"-e", fmt.Sprintf("CB_BIGQUERY_URL=http://%s:%d/bigquery/v2/", host, bigqueryPort),
		"-e", "GOOGLE_CLOUD_PROJECT=" + job.Project,
	}
}

// launcherEvent is one line the launcher prints.
type launcherEvent struct {
	Event      string      `json:"event"`
	State      string      `json:"state"`
	Message    string      `json:"message"`
	Importance string      `json:"importance"`
	Text       string      `json:"text"`
	Transforms []Transform `json:"transforms"`
	Metrics    []Metric    `json:"metrics"`
}

func (d *Docker) read(r io.Reader, ev Events, workers int) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var res Result
	started := false
	for sc.Scan() {
		line := sc.Text()
		var e launcherEvent
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &e) != nil || e.Event == "" {
			if strings.TrimSpace(line) != "" {
				res.tail = append(res.tail, line)
				if len(res.tail) > 3 {
					res.tail = res.tail[1:]
				}
			}
			if ev.Log != nil && strings.TrimSpace(line) != "" {
				ev.Log(severityOf(line), line)
			}
			continue
		}
		switch e.Event {
		case "graph":
			if ev.Graph != nil {
				ev.Graph(e.Transforms)
			}
		case "state":
			if e.State == "RUNNING" && !started {
				started = true
				if ev.Started != nil {
					ev.Started(workers)
				}
			}
		case "metrics":
			if ev.Metrics != nil {
				ev.Metrics(e.Metrics)
			}
		case "message":
			if ev.Message != nil && e.Text != "" {
				ev.Message(e.Importance, e.Text)
			}
		case "done":
			res = Result{State: e.State, Message: e.Message, tail: res.tail}
			if res.State == "FAILED" && res.Message == "" {
				res.Message = "Workflow failed."
			}
		}
	}
	return res
}

var severityRE = regexp.MustCompile(`\b(DEBUG|INFO|WARNING|WARN|ERROR|CRITICAL|SEVERE)\b`)

func severityOf(line string) string {
	switch m := severityRE.FindString(line); m {
	case "WARN":
		return "WARNING"
	case "SEVERE", "CRITICAL":
		return "ERROR"
	case "":
		return "INFO"
	default:
		return m
	}
}

// follow sends each line of container's output to log.
func (d *Docker) follow(ctx context.Context, container string, log func(severity, text string)) {
	if log == nil {
		return
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", container)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if cmd.Start() != nil {
		return
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			log(severityOf(t), t)
		}
	}
	_ = cmd.Wait()
}

func (d *Docker) waitLog(ctx context.Context, name, want string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		logs, _ := d.docker(ctx, "logs", name)
		if strings.Contains(logs, want) {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the job's relay did not start: %s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// launcherMetadata is the live address jobNetwork routes for a provided
// Flex Template's launcher only: the instance's metadata server
// (metadataPort).
const launcherMetadata = "launcher-metadata"

// googleHosts are the Google API hosts a job's containers (its launcher,
// job server and workers) resolve to its relay, each answered by the
// instance service (its live address's name) it is, through the relay's
// certificate, which names them: a client built at its default endpoint
// reaches the instance as on Google it reaches Google. BigQuery to
// Parquet's launcher reads its table's schema through the BigQuery
// Storage API's v1beta1 client (BigQueryStorageClient.create()) before
// the job is submitted; Beam's SpannerIO of a template that has no
// spannerHost (Spanner to BigQuery) dials batch-spanner.googleapis.com
// (SpannerConfig's default host). The job's network has no other route,
// so nothing reaches Google.
var googleHosts = map[string]string{
	"bigquerystorage.googleapis.com": "bigquery-storage",
	"batch-spanner.googleapis.com":   "spanner",
	"spanner.googleapis.com":         "spanner",
}

// jobGoogleHosts are the googleHosts whose service the instance serves
// (live), sorted.
func jobGoogleHosts(live map[string]string) []string {
	var out []string
	for h, svc := range googleHosts {
		if live[svc] != "" {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// googleHostArgs are docker run's --add-host flags resolving the
// googleHosts the instance serves to the relay.
func googleHostArgs(live map[string]string, relayIP string) []string {
	var args []string
	for _, h := range jobGoogleHosts(live) {
		args = append(args, "--add-host", h+":"+relayIP)
	}
	return args
}

// writeRelayTLS writes a CA made for the job (ca.pem), and the relay's
// certificate of relayName and of names signed by it (cert.pem,
// key.pem), into dir.
func writeRelayTLS(dir string, names ...string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "CloudBurrow Dataflow job CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano() + 1), Subject: pkix.Name{CommonName: relayName},
		DNSNames: append([]string{relayName}, names...), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	for name, block := range map[string]*pem.Block{"ca.pem": {Type: "CERTIFICATE", Bytes: caDER},
		"cert.pem": {Type: "CERTIFICATE", Bytes: der}, "key.pem": {Type: "PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeAssets(dir string) error {
	for _, name := range []string{"relay.py", "py/cblaunch.py", "py/cbio.py", "py/sitecustomize.py", "py/cbfnproxy.py", "py/cbnet.py", "java/pool.sh", "java/CbCredentialFactory.java", "java/GreedyPCollectionFusers.java", "java/FlinkBatchPortablePipelineTranslator.java", "java/ExecutableStageDoFnOperator.java"} {
		b, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// hostTargets are host addresses as a container on the default bridge
// reaches them: host.docker.internal on Docker Desktop; on Docker Engine
// a relay on the bridge's gateway (internal/hostrelay), as the Spark
// runner does.
func (d *Docker) hostTargets(ctx context.Context, addrs ...string) ([]string, func(), error) {
	if err := d.probeDesktop(); err != nil {
		return nil, nil, err
	}
	out := make([]string, len(addrs))
	if d.desktop {
		for i, a := range addrs {
			_, port, _ := net.SplitHostPort(a)
			out[i] = "host.docker.internal:" + port
		}
		return out, func() {}, nil
	}
	gw, err := d.docker(ctx, "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return nil, nil, errors.New("find the default bridge's gateway: " + gw)
	}
	var ip string
	for _, f := range strings.Fields(gw) {
		if p := net.ParseIP(f); p != nil && p.To4() != nil {
			ip = f
			break
		}
	}
	if ip == "" {
		return nil, nil, fmt.Errorf("the default bridge has no IPv4 gateway (%q)", gw)
	}
	var relays []*hostrelay.Relay
	stop := func() {
		for _, r := range relays {
			_ = r.Close()
		}
	}
	for i, a := range addrs {
		r := &hostrelay.Relay{Listen: net.JoinHostPort(ip, "0"), Target: a}
		if err := r.Start(ctx); err != nil {
			stop()
			return nil, nil, err
		}
		relays = append(relays, r)
		out[i] = r.Addr()
	}
	return out, stop, nil
}

func (d *Docker) probeDesktop() error {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.probed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := d.docker(ctx, "info", "--format", "{{.OperatingSystem}}")
	if err != nil {
		return errors.New("ask Docker which daemon it is: " + out)
	}
	d.desktop, d.probed = strings.Contains(out, "Docker Desktop"), true
	return nil
}

// RemoveInstance removes every container and network labelled for
// instance, as a run's cleanup would have: for `cloudburrow delete` and a
// run the CLI's exit cut short.
func RemoveInstance(ctx context.Context, instance string) {
	if instance == "" {
		return
	}
	label := "label=" + InstanceLabel + "=" + instance
	// One removal of them all: Docker removes the containers of one
	// command concurrently, where one command each took a second or more
	// apiece (a job has up to nine).
	out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", label).Output()
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f", "-v"}, ids...)...).Run()
	}
	out, _ = exec.CommandContext(ctx, "docker", "network", "ls", "-q", "--filter", label).Output()
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"network", "rm"}, ids...)...).Run()
	}
}

// Shutdown marks the instance as stopping: a run cancelled from now on
// removes its containers at once (Run) rather than cancelling its job
// through the runner, which for a streaming job can take longer than the
// instance's whole stop may.
func (d *Docker) Shutdown() { d.closing.Store(true) }
