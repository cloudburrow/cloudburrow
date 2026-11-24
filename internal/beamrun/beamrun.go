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
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

//go:embed assets/relay.py assets/py/cblaunch.py assets/py/cbio.py assets/py/sitecustomize.py
var assets embed.FS

// The pinned images (dependencies.json components.dataflow).
const (
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
	relayName      = "relay.cloudburrow.localhost"
	storagePort    = 9000
	pubsubPort     = 9001
	bigqueryPort   = 9002
	dataflowPort   = 80
	workerPoolPort = 50000
)

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
}

// Result is how a job ended: State DONE, FAILED or CANCELLED.
type Result struct {
	State, Message string
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
	Logf  func(format string, args ...any)

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
	if job.SDK != "Python" {
		return failed(fmt.Sprintf("The job was submitted by the Apache Beam %s SDK: CloudBurrow's local runner serves the Python SDK's pipelines (Apache Beam %s, Python %s).", job.SDK, BeamVersion, strings.Join(pythonVersions(), ", ")))
	}
	image := PythonImages[job.PythonVersion]
	if image == "" || job.SDKVersion != BeamVersion {
		return failed(fmt.Sprintf("The job was submitted by Apache Beam %s on Python %s: CloudBurrow's local runner has the SDK harness of Apache Beam %s on Python %s only.", job.SDKVersion, job.PythonVersion, BeamVersion, strings.Join(pythonVersions(), ", ")))
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
	for _, sub := range []string{"ctl", "metrics"} {
		_ = os.Mkdir(filepath.Join(dir, sub), 0o777)
		_ = os.Chmod(filepath.Join(dir, sub), 0o777)
	}

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

	var routes []string
	var targets []string
	ports := []int{}
	for _, r := range []struct {
		name string
		port int
	}{{"storage", storagePort}, {"pubsub", pubsubPort}, {"bigquery", bigqueryPort}, {"dataflow", dataflowPort}} {
		if a := live[r.name]; a != "" {
			targets = append(targets, a)
			ports = append(ports, r.port)
		}
	}
	host, stop, err := d.hostTargets(ctx, targets...)
	if err != nil {
		return failed(err.Error())
	}
	cleanup = append(cleanup, stop)
	for i, p := range ports {
		routes = append(routes, fmt.Sprintf("%d=%s", p, host[i]))
	}

	if out, err := d.docker(ctx, "network", "create", "--internal", "--label", label, network); err != nil {
		return failed("create the job's internal network: " + out)
	}
	cleanup = append(cleanup, func() { bg("network", "rm", network) })
	relayArgs := []string{"run", "-d", "--name", relay, "--label", label, "-v", dir + ":/cb:ro", "--entrypoint", "python3"}
	if !d.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	relayArgs = append(relayArgs, image, "/cb/relay.py")
	relayArgs = append(relayArgs, routes...)
	if out, err := d.docker(ctx, relayArgs...); err != nil {
		return failed("start the job's relay: " + out)
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", relay) })
	if err := d.waitLog(ctx, relay, "ready", 2*time.Minute); err != nil {
		return failed(err.Error())
	}
	if out, err := d.docker(ctx, "network", "connect", "--alias", relayName, network, relay); err != nil {
		return failed("connect the job's relay: " + out)
	}

	parallelism := job.Workers
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > MaxParallelism {
		parallelism = MaxParallelism
	}
	if out, err := d.docker(ctx, "run", "-d", "--name", js, "--label", label, "--network", network, "--memory", "3g",
		JobServerImage, "--flink-master=[local]", "--job-host=0.0.0.0"); err != nil {
		return failed("start the Flink job server: " + out)
	}
	cleanup = append(cleanup, func() { bg("rm", "-f", "-v", js) })
	env := d.clientEnv(job, dir)
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

	runArgs := append([]string{"run", "--rm", "--name", launch, "--label", label, "--network", network, "-v", dir + ":/cb"}, env...)
	runArgs = append(runArgs, "--entrypoint", "python", image, "/cb/py/cblaunch.py",
		"--job-endpoint", js+":8099", "--artifact-endpoint", js+":8098",
		"--pipeline-url", job.PipelineURL, "--options", "/cb/options.json", "--job-name", job.Name,
		"--parallelism", fmt.Sprint(parallelism), "--cancel-file", "/cb/ctl/cancel")
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
		return failed(msg)
	}
	return res
}

func pythonVersions() []string {
	var out []string
	for v := range PythonImages {
		out = append(out, v)
	}
	return out
}

// clientEnv is the job's clients' environment on its network.
func (d *Docker) clientEnv(job Job, dir string) []string {
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
		"-e", fmt.Sprintf("PUBSUB_EMULATOR_HOST=%s:%d", host, pubsubPort),
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
			res = Result{State: e.State, Message: e.Message}
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

func writeAssets(dir string) error {
	for _, name := range []string{"relay.py", "py/cblaunch.py", "py/cbio.py", "py/sitecustomize.py"} {
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
	out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", label).Output()
	for _, id := range strings.Fields(string(out)) {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", id).Run()
	}
	out, _ = exec.CommandContext(ctx, "docker", "network", "ls", "-q", "--filter", label).Output()
	for _, id := range strings.Fields(string(out)) {
		_ = exec.CommandContext(ctx, "docker", "network", "rm", id).Run()
	}
}
