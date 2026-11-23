package sparkproc

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
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

//go:embed assets/relay.py assets/procedure.py assets/run.sh assets/node.sh assets/eco.sh assets/hivenode.sh assets/nodemanager.sh assets/hdfs.sh
var assets embed.FS

// The connector's endpoints, the relay's aliases on the job's network.
const (
	// restHost is the name the instance's REST ports answer to beside
	// loopback (hostguard, ADR-0004), as Docker Desktop names the host.
	restHost    = "host.docker.internal"
	storageHost = "bigquerystorage.cloudburrow.internal"
	restPort    = "8080"
	storagePort = "8443"
	// accessToken is the connector's gcpAccessToken: the front takes every
	// caller to be the instance's one identity, so any token serves, and a
	// fixed one is never refreshed.
	accessToken = "cloudburrow-spark-procedure"
)

// LogEntry is one line of the application's output, for Cloud Logging.
type LogEntry struct {
	Job       Job
	Component string // DRIVER or EXECUTOR
	Severity  string
	Text      string
	Time      time.Time
	// Spark is set for Spark's own log4j lines, beside the application's.
	Spark bool
}

// Runtime is one runtime version's Spark: the official apache/spark
// image of its Spark and Java, by digest, and Google's BigQuery connector
// for that Spark.
type Runtime struct {
	Image string
	// RImage is the same Spark's image with R beside Python (the
	// official image's -python3-r- variant), which SparkR workloads and
	// Dataproc clusters run in; "" when none is pinned.
	RImage string
	// Kernel is how a Jupyter session's kernel image is built on Image
	// (KernelImage), nil when none is pinned for this runtime's Spark.
	Kernel *KernelRuntime
	// ConnectServer returns the Spark Connect server's jar when the
	// image's Spark does not bundle it (Spark 3.5), nil when it does.
	ConnectServer func(ctx context.Context) (string, error)
	// Connector returns the connector jar's path on the host, fetching
	// it on the first use.
	Connector func(ctx context.Context) (string, error)
}

// Docker runs jobs in the Spark image.
type Docker struct {
	// Runtimes are the served runtime versions (MAJOR.MINOR); Default
	// the one a procedure with no runtime_version runs on.
	Runtimes map[string]Runtime
	Default  string
	// Clusters are the Dataproc cluster image versions (MAJOR.MINOR) a
	// cluster runs (#2162): each one's Spark, its RImage the nodes'.
	Clusters map[string]Runtime
	// IPython returns the host directory of IPython and its
	// dependencies' packages (pure Python), mounted on a notebook batch's
	// PYTHONPATH (Job.IPython), fetching them on the first use; nil
	// serves none.
	IPython func(ctx context.Context) (string, error)
	// History is the host directory batches and sessions write their
	// Spark event logs to, which the instance's History Server reads
	// (their Spark UI); "" writes none.
	History string
	// BigQuery and Storage are the instance's BigQuery REST and Storage
	// API addresses on the host (host:port), live at each run; Registry
	// its Artifact Registry's (the Docker registry a container_image is
	// read from), or nil.
	BigQuery, Storage, Registry func() string
	// GCS is the instance's Cloud Storage (JSON API) address on the
	// host, which a cluster's Hadoop, Hive and Pig reach gs:// through;
	// nil or "" for none.
	GCS func() string
	// Ecosystem is what a cluster's Hadoop, Hive, Pig, Trino and Flink
	// jobs and its optional components run on; nil serves none.
	Ecosystem *Ecosystem
	// Mint mints a token of the instance for a service account that
	// exists, for a job's RunAs; nil refuses RunAs.
	Mint func(email string) (string, error)
	// KeyCheck checks a job's customer-managed key for its agent
	// (cmek.Keys.Usable on the host's Cloud KMS), or is nil, when a job
	// with a key is refused: the instance serves no Cloud KMS.
	KeyCheck func(ctx context.Context, project, agent, key string) error
	// Instance labels the containers and networks made.
	Instance string
	// Tmp holds each job's directory ("": the system's); Cache the Spark
	// and Java trees a custom container is given ("": under Tmp).
	Tmp, Cache string
	// Disks holds the Dataproc clusters' nodes' disks, by cluster UUID
	// and host name, which outlive the instance's restart ("": each
	// cluster's own directory, removed with it).
	Disks string
	// Log receives each line of output; Logf the runner's own notes.
	Log  func(LogEntry)
	Logf func(format string, args ...any)

	// desktop says whether the daemon is Docker Desktop; probed is set
	// once a probe has answered (a failed probe is retried next run).
	probeMu sync.Mutex
	probed  bool
	desktop bool
	treeMu  sync.Mutex
}

func (d *Docker) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

var nameSafe = regexp.MustCompile(`[^a-z0-9-]+`)

// runtime is the job's runtime: its runtime_version's MAJOR.MINOR, or
// the default.
func (d *Docker) runtime(job Job) (Runtime, string, bool) {
	v := job.RuntimeVersion
	if v == "" {
		v = d.Default
	}
	if parts := strings.Split(v, "."); len(parts) > 2 {
		v = parts[0] + "." + parts[1]
	}
	rt, ok := d.Runtimes[v]
	return rt, v, ok && rt.Image != "" && rt.Connector != nil
}

// Run runs job to its end.
func (d *Docker) Run(ctx context.Context, job Job) (Result, error) {
	if job.KMSKey != "" {
		if res, refused := d.checkKey(ctx, job); refused {
			return res, nil
		}
	}
	if job.KeyOnly {
		return Result{}, nil
	}
	p, failed, err := d.prepare(ctx, job)
	if err != nil || failed != nil {
		if failed != nil {
			return *failed, nil
		}
		return Result{}, err
	}
	defer p.close()
	d.logf("spark: running %s (%s) on runtime %s in %s", job.Procedure, job.SparkJobID, p.version, p.image)
	cmd := exec.CommandContext(ctx, "docker", p.run...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return Result{}, &Unavailable{Msg: "start the Spark container: " + err.Error()}
	}
	tail, output := d.follow(job, stdout, nil)
	waitErr := cmd.Wait()
	res := Result{Out: map[string]string{}, DriverOutput: output}
	if b, err := os.ReadFile(filepath.Join(p.dir, "out", "params.json")); err == nil {
		_ = json.Unmarshal(b, &res.Out)
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return Result{}, &Unavailable{Msg: "the Spark container: " + waitErr.Error()}
		}
		if exit.ExitCode() == 125 {
			return Result{}, &Unavailable{Msg: "the Spark container did not start: " + strings.Join(tail(), "\n")}
		}
		res.Failed = true
		res.Message = failure(tail())
	}
	return res, nil
}

// prepared is a job made ready to run: its directory, its network and
// relay up, and the docker run arguments of its Spark container.
type prepared struct {
	dir, network, relay, spark string
	image, version             string
	run                        []string
	cleanup                    []func()
}

// close removes the container, the relay, the network and the directory.
func (p *prepared) close() {
	for i := len(p.cleanup) - 1; i >= 0; i-- {
		p.cleanup[i]()
	}
	p.cleanup = nil
}

// prepare readies job: an error is an *Unavailable (or a host failure),
// and failed a Result the job ends with before it runs (a RunAs that
// cannot be minted, a custom container that cannot be read). inbound,
// when set, is a port of the Spark container the relay publishes on the
// host's loopback (a Spark Connect server's).
func (d *Docker) prepare(ctx context.Context, job Job, inbound ...string) (_ *prepared, failed *Result, _ error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil, &Unavailable{Msg: "docker is not on PATH: Apache Spark workloads run in a local Spark container"}
	}
	rt, version, ok := d.runtime(job)
	if !ok {
		return nil, nil, &Unavailable{Msg: "the Spark runner serves no runtime version " + version}
	}
	jar, err := rt.Connector(ctx)
	if err != nil {
		return nil, nil, &Unavailable{Msg: "the BigQuery connector for Spark is not available: " + err.Error()}
	}
	rest, storage := d.BigQuery(), d.Storage()
	if rest == "" || storage == "" {
		return nil, nil, &Unavailable{Msg: "the instance's BigQuery REST and Storage API ports are not live"}
	}
	token := accessToken
	switch {
	case job.RunAs != "":
		if d.Mint == nil {
			return nil, nil, &Unavailable{Msg: "the Spark runner cannot mint a token for " + job.RunAs}
		}
		tok, err := d.Mint(job.RunAs)
		if err != nil {
			return nil, &Result{Failed: true, Message: err.Error()}, nil
		}
		token = tok
	case job.AccessToken != "":
		token = job.AccessToken
	}
	var custom *customImage
	if job.ContainerImage != "" {
		ci, err := d.customImage(ctx, job.ContainerImage, rt.Image)
		if err != nil {
			var u *Unavailable
			if errors.As(err, &u) {
				return nil, nil, err
			}
			return nil, &Result{Failed: true, Message: err.Error()}, nil
		}
		custom = ci
	}
	p := &prepared{version: version}
	ok = false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	dir, err := os.MkdirTemp(d.Tmp, "cb-spark-")
	if err != nil {
		return nil, nil, err
	}
	p.dir = dir
	p.cleanup = append(p.cleanup, func() { _ = os.RemoveAll(dir) })
	args, err := writeJob(dir, job)
	if err != nil {
		return nil, nil, err
	}
	if err := writeToken(dir, token); err != nil {
		return nil, nil, err
	}
	if err := writeTLS(filepath.Join(dir, "tls")); err != nil {
		return nil, nil, err
	}
	// Readable by the image's spark user (uid 185); out/ writable.
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
	suffix = strings.Trim(suffix, "-") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
	network, relay, spark := "cb-spark-net-"+suffix, "cb-spark-relay-"+suffix, "cb-spark-"+suffix
	p.network, p.relay, p.spark = network, relay, spark
	label := d.instanceLabel()

	restTarget, storageTarget, stop, err := d.targets(ctx, rest, storage)
	if err != nil {
		return nil, nil, &Unavailable{Msg: err.Error()}
	}
	p.cleanup = append(p.cleanup, stop)

	if out, err := d.docker(ctx, "network", "create", "--internal", "--label", label, network); err != nil {
		return nil, nil, &Unavailable{Msg: "create the Spark job's internal network: " + out}
	}
	p.cleanup = append(p.cleanup, func() { _, _ = d.docker(context.Background(), "network", "rm", network) })

	relayArgs := []string{"run", "-d", "--name", relay, "--label", label, "-v", dir + ":/cb:ro", "--entrypoint", "python3"}
	if !d.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	for _, port := range inbound {
		// Published from the default bridge, the relay's first network:
		// a container on an --internal network publishes nothing.
		relayArgs = append(relayArgs, "-p", "127.0.0.1::"+port)
	}
	relayArgs = append(relayArgs, rt.Image, "/cb/relay.py", restTarget, storageTarget)
	for _, port := range inbound {
		relayArgs = append(relayArgs, spark+":"+port)
	}
	if out, err := d.docker(ctx, relayArgs...); err != nil {
		return nil, nil, &Unavailable{Msg: "start the Spark job's relay: " + out}
	}
	p.cleanup = append(p.cleanup, func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", relay) })
	if err := d.waitReady(ctx, relay); err != nil {
		return nil, nil, &Unavailable{Msg: err.Error()}
	}
	if out, err := d.docker(ctx, "network", "connect", "--alias", restHost, "--alias", storageHost, network, relay); err != nil {
		return nil, nil, &Unavailable{Msg: "connect the Spark job's relay: " + out}
	}

	run := []string{"run", "--name", spark, "--label", label, "--network", network,
		"-v", dir + ":/cb", "-v", jar + ":/cb-connector/spark-bigquery.jar:ro",
		"-e", "CB_SPARK_OUT=/cb/out/params.json", "-e", "CB_SPARK_TYPES=/cb/types.json"}
	image, conf := rt.Image, map[string]string{}
	if job.Kind == KindSparkR {
		if rt.RImage == "" {
			return nil, &Result{Failed: true, Message: "SparkR is not available on runtime " + version + ": no Spark image with R is pinned for it"}, nil
		}
		image = rt.RImage
	}
	if d.History != "" && job.Kind != "" {
		// The event log the Spark UI is read from (#2162).
		run = append(run, "-v", d.History+":/cb-history")
		conf["spark.eventLog.enabled"], conf["spark.eventLog.dir"] = "true", "file:/cb-history"
		if job.AppID != "" {
			// Spark's local scheduler names its application local-MILLIS
			// whatever spark.app.id says, so the workload's ID is a
			// property of its own, which the event log's environment
			// records and the Spark UI's path is resolved by.
			conf[WorkloadProperty] = job.AppID
		}
	}
	pythonPath := "/cb/lib"
	if job.IPython {
		if d.IPython == nil {
			return nil, nil, &Unavailable{Msg: "the Spark runner serves no IPython for notebook batches"}
		}
		ip, err := d.IPython(ctx)
		if err != nil {
			return nil, nil, &Unavailable{Msg: "IPython is not available: " + err.Error()}
		}
		run = append(run, "-v", ip+":/cb-ipython:ro")
		pythonPath += ":/cb-ipython"
	}
	if custom != nil && custom.env["PYTHONPATH"] != "" {
		pythonPath += ":" + custom.env["PYTHONPATH"] // the image's packages too
	}
	run = append(run, "-e", "PYTHONPATH="+pythonPath)
	if custom != nil {
		// As Google's runtime does: Spark at /usr/lib/spark and the
		// runtime's Java mounted into the image, which holds neither, run
		// as the spark user (UID and GID 1099); the image's own
		// PYSPARK_PYTHON wins, and its SPARK_EXTRA_CLASSPATH is on the
		// driver's and executors' class path.
		image = custom.ref
		run = append(run, "--user", "1099:1099", "-v", custom.spark+":/usr/lib/spark:ro", "-v", custom.java+":"+customJavaHome+":ro",
			"-e", "JAVA_HOME="+customJavaHome, "-e", "SPARK_HOME=/usr/lib/spark", "-e", "SPARK_CONF_DIR=/etc/spark/conf", "-e", "HOME=/tmp")
		if custom.env["PYSPARK_PYTHON"] == "" {
			run = append(run, "-e", "PYSPARK_PYTHON=python3")
		}
		if cp := custom.env["SPARK_EXTRA_CLASSPATH"]; cp != "" {
			conf["spark.driver.extraClassPath"], conf["spark.executor.extraClassPath"] = cp, cp
		}
	} else {
		run = append(run, "-e", "PYSPARK_PYTHON=python3")
	}
	names := make([]string, 0, len(job.Params))
	for name := range job.Params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		run = append(run, "-e", "BIGQUERY_PROC_PARAM."+name+"="+job.Params[name])
	}
	if job.Kind == KindConnect && rt.ConnectServer != nil {
		// Spark 3.5's distribution has no Spark Connect server jar
		// (spark-connect_2.12, which start-connect-server.sh fetches
		// with --packages): it is pinned instead.
		server, err := rt.ConnectServer(ctx)
		if err != nil {
			p.close()
			return nil, nil, &Unavailable{Msg: "the Spark Connect server jar is not available: " + err.Error()}
		}
		run = append(run, "-v", server+":/cb-connector/spark-connect.jar:ro")
		args.jars = append(args.jars, "/cb-connector/spark-connect.jar")
	}
	run = append(run, "--entrypoint", "/bin/bash", image, "/cb/run.sh")
	run = append(run, submitArgs(job, args, token, conf)...)
	p.image, p.run = image, run
	p.cleanup = append(p.cleanup, func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", spark) })
	ok = true
	return p, nil, nil
}

// targets are the relay's two targets, as the relay container reaches
// the host. Docker Desktop's host.docker.internal is the host's loopback;
// on Docker Engine the host's loopback is not reachable from a container,
// so the runner relays the two ports from the default bridge's gateway
// address for the run (internal/hostrelay, as #575 does for the cluster).
func (d *Docker) targets(ctx context.Context, rest, storage string) (string, string, func(), error) {
	t, stop, err := d.hostTargets(ctx, rest, storage)
	if err != nil {
		return "", "", nil, err
	}
	return t[0], t[1], stop, nil
}

// hostTargets are host addresses (host:port on the host's loopback) as
// a relay container reaches them, in order, and the function that stops
// the host relays made for them.
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

// probeDesktop asks the daemon whether it is Docker Desktop. The probe
// has its own deadline rather than a job's context: a job cancelled
// while it ran must not leave every later job relaying the wrong way.
// Only an answer is kept, so a failed probe is retried by the next job.
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
		return &Unavailable{Msg: "ask Docker which daemon it is: " + out}
	}
	d.desktop, d.probed = strings.Contains(out, "Docker Desktop"), true
	return nil
}

func (d *Docker) docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (d *Docker) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		logs, _ := d.docker(ctx, "logs", name)
		if strings.Contains(logs, "ready") {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the Spark job's relay did not start: %s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// log4j's console layout in the image: "25/11/21 10:00:00 INFO Executor: …".
var log4jLine = regexp.MustCompile(`^\d\d/\d\d/\d\d \d\d:\d\d:\d\d (TRACE|DEBUG|INFO|WARN|ERROR|FATAL) (\S+?):`)

// follow sends each line of r to Log, and returns the last lines read
// and the output's last MaxDriverOutput bytes.
func (d *Docker) follow(job Job, r io.Reader, watch func(string)) (func() []string, string) {
	var mu sync.Mutex
	var last []string
	var all []byte
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		component, severity, spark := "DRIVER", "INFO", false
		if m := log4jLine.FindStringSubmatch(line); m != nil {
			spark = true
			severity = map[string]string{"TRACE": "DEBUG", "FATAL": "CRITICAL", "WARN": "WARNING"}[m[1]]
			if severity == "" {
				severity = m[1]
			}
			if strings.HasPrefix(m[2], "Executor") || strings.Contains(m[2], "executor.") {
				component = "EXECUTOR"
			}
		}
		if d.Log != nil {
			d.Log(LogEntry{Job: job, Component: component, Severity: severity, Text: line, Time: time.Now(), Spark: spark})
		}
		if watch != nil {
			watch(line)
		}
		mu.Lock()
		last = append(last, line)
		if len(last) > 400 {
			last = last[len(last)-400:]
		}
		all = append(all, line...)
		all = append(all, '\n')
		if len(all) > MaxDriverOutput {
			all = append(all[:0], all[len(all)-MaxDriverOutput:]...)
		}
		mu.Unlock()
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), last...)
	}, string(all)
}

// pyError is a Python traceback's last line: "ValueError: kaboom".
var pyError = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*(Error|Exception|Exit|Interrupt)\b`)

// failure is a failed application's error from its last lines: a Python
// traceback's last line (for py4j's, the Java exception it carries, on
// the line after), or the JVM's uncaught exception, or else the last
// ERROR line.
func failure(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimRight(lines[i], " \t")
		if !pyError.MatchString(l) {
			continue
		}
		if strings.HasPrefix(l, "py4j.protocol.Py4JJavaError") && i+1 < len(lines) {
			if next := strings.TrimSpace(lines[i+1]); strings.HasPrefix(next, ": ") {
				return strings.TrimPrefix(next, ": ")
			}
		}
		return l
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if _, ex, ok := strings.Cut(lines[i], `Exception in thread "main" `); ok {
			return strings.TrimSpace(ex)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(l, " ERROR ") {
			return l
		}
	}
	return "The Spark application failed"
}

// jobArgs are a job's files as spark-submit names them in the container.
type jobArgs struct {
	main                         string
	pyFiles, jars, files, archiv []string
}

// writeJob writes the job's files under dir and the library and scripts
// the container runs.
func writeJob(dir string, job Job) (jobArgs, error) {
	var a jobArgs
	put := func(rel string, data []byte) (string, error) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return "/cb/" + rel, os.WriteFile(p, data, 0o644)
	}
	for _, name := range []string{"relay.py", "run.sh"} {
		b, _ := assets.ReadFile("assets/" + name)
		if _, err := put(name, b); err != nil {
			return a, err
		}
	}
	lib, _ := assets.ReadFile("assets/procedure.py")
	for rel, data := range map[string][]byte{"lib/bigquery/__init__.py": nil, "lib/bigquery/spark/__init__.py": nil,
		"lib/bigquery/spark/procedure.py": lib} {
		if _, err := put(rel, data); err != nil {
			return a, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o777); err != nil {
		return a, err
	}
	types, _ := json.Marshal(job.Types)
	if _, err := put("types.json", types); err != nil {
		return a, err
	}
	var err error
	switch {
	case job.Kind == KindSparkSQL && job.Main != nil:
		// query_variables, each "equivalent to the Spark SQL command: SET
		// name="value";" (the API reference), set before the query file.
		var pre strings.Builder
		vars := make([]string, 0, len(job.QueryVariables))
		for k := range job.QueryVariables {
			vars = append(vars, k)
		}
		sort.Strings(vars)
		for _, k := range vars {
			fmt.Fprintf(&pre, "SET %s=\"%s\";\n", k, job.QueryVariables[k])
		}
		data := job.Main.Data
		if local := localPath(job.Main.URI); local != "" {
			pre.WriteString("SOURCE " + local + ";\n")
			data = nil
		}
		a.main, err = put("main/query.sql", append([]byte(pre.String()), data...))
	case job.Main != nil && localPath(job.Main.URI) != "":
		a.main = localPath(job.Main.URI)
	case job.Main != nil:
		a.main, err = put("main/"+safeBase(job.Main.Base()), job.Main.Data)
	case job.Code != "":
		a.main, err = put("main/main.py", []byte(job.Code))
	}
	if err != nil {
		return a, err
	}
	for kind, files := range map[string][]File{"py": job.PyFiles, "jars": job.Jars, "files": job.Files, "archives": job.Archives} {
		for i, f := range files {
			p := localPath(f.URI)
			if p == "" {
				p, err = put(fmt.Sprintf("%s/%d/%s", kind, i, safeBase(f.Base())), f.Data)
			}
			if err != nil {
				return a, err
			}
			switch kind {
			case "py":
				a.pyFiles = append(a.pyFiles, p)
			case "jars":
				a.jars = append(a.jars, p)
			case "files":
				a.files = append(a.files, p)
			case "archives":
				a.archiv = append(a.archiv, p)
			}
		}
	}
	sort.Strings(a.pyFiles)
	sort.Strings(a.jars)
	sort.Strings(a.files)
	sort.Strings(a.archiv)
	return a, nil
}

// UsrLibSpark is where the runtime's own files are, as a batch names
// them (file:///usr/lib/spark/examples/jars/spark-examples.jar): run.sh
// links the image's Spark there under the names Google's runtime has.
const usrLibSpark = "/tmp/cb-usr-lib-spark/"

// localPath is a file:// URI's path in the Spark container, a file of the
// runtime rather than of Cloud Storage, or "" for any other URI.
func localPath(uri string) string {
	p, ok := strings.CutPrefix(uri, "file://")
	if !ok || !strings.HasPrefix(p, "/") {
		return ""
	}
	if rest, ok := strings.CutPrefix(p, "/usr/lib/spark/"); ok {
		return usrLibSpark + rest
	}
	return p
}

func safeBase(s string) string {
	s = filepath.Base(filepath.Clean("/" + s))
	if s == "/" || s == "." || s == "" {
		return "file"
	}
	return s
}

// submitArgs are spark-submit's arguments for job: the connector sends
// token, and extra are Spark properties the procedure's own override.
func submitArgs(job Job, a jobArgs, token string, extra map[string]string) []string {
	// The token is in a properties file, never on a command line.
	args := []string{"--master", "local[*]", "--name", job.SparkJobID, "--properties-file", "/cb/token/spark.conf",
		"--driver-java-options", "-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks -Djavax.net.ssl.trustStorePassword=changeit",
		"--jars", strings.Join(append([]string{"/cb-connector/spark-bigquery.jar"}, a.jars...), ",")}
	if len(a.pyFiles) > 0 {
		args = append(args, "--py-files", strings.Join(a.pyFiles, ","))
	}
	if len(a.files) > 0 {
		args = append(args, "--files", strings.Join(a.files, ","))
	}
	if len(a.archiv) > 0 {
		args = append(args, "--archives", strings.Join(a.archiv, ","))
	}
	conf := map[string]string{
		"spark.datasource.bigquery.bigQueryHttpEndpoint":        "http://" + restHost + ":" + restPort,
		"spark.datasource.bigquery.bigQueryStorageGrpcEndpoint": storageHost + ":" + storagePort,
		"spark.datasource.bigquery.parentProject":               job.Project,
		"spark.datasource.bigquery.project":                     job.Project,
		"spark.ui.enabled":                                      "false",
	}
	if job.Kind == KindConnect {
		conf["spark.connect.grpc.binding.port"] = ConnectPort
	}
	for k, v := range extra {
		conf[k] = v
	}
	for k, v := range job.Properties {
		conf[k] = v
	}
	keys := make([]string, 0, len(conf))
	for k := range conf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--conf", k+"="+conf[k])
	}
	switch job.Kind {
	case KindSparkSQL:
		// What bin/spark-sql runs, the query file its -f.
		return append(args, "--class", "org.apache.spark.sql.hive.thriftserver.SparkSQLCLIDriver", "spark-internal", "-f", a.main)
	case KindConnect:
		// What sbin/start-connect-server.sh runs, in the foreground.
		return append(args, "--class", "org.apache.spark.sql.connect.service.SparkConnectServer", "spark-internal")
	}
	if job.MainClass != "" {
		args = append(args, "--class", job.MainClass)
		if a.main == "" && len(a.jars) > 0 {
			// main_class in jar_uris: spark-submit runs the first jar.
			a.main = a.jars[0]
		}
	}
	return append(append(args, a.main), job.Args...)
}

// writeToken writes the connector's token as the application's
// properties file (--properties-file), so it is never on a command line.
func writeToken(dir, token string) error {
	p := filepath.Join(dir, "token")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p, "spark.conf"), []byte("spark.datasource.bigquery.gcpAccessToken "+token+"\n"), 0o644)
}

// writeTLS writes a CA made for the run and the relay's certificate of
// the Storage API's name, signed by it.
func writeTLS(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "CloudBurrow Spark procedure CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(7 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano() + 1), Subject: pkix.Name{CommonName: storageHost},
		DNSNames: []string{storageHost, restHost}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(7 * 24 * time.Hour),
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
