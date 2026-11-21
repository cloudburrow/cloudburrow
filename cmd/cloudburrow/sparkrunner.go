package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// sparkRuntime is one runtime version of BigQuery's stored procedures for
// Apache Spark (#2104), pinned in dependencies.json
// (components.bigqueryml.spark*): the official apache/spark image of the
// Spark and Java of that Managed Service for Apache Spark runtime, and
// Google's BigQuery connector for that Spark from Maven Central.
type sparkRuntime struct {
	image                       string
	connectorURL, connectorSHA  string
	connectorFile, sparkVersion string
}

// sparkRuntimes are the runtime versions, as Managed Service for Apache
// Spark's runtime pages list their components (re-read 2026-11-21:
// https://docs.cloud.google.com/managed-spark/docs/concepts/versions/spark-runtime-2.3
// and its 1.2, 2.2, 3.0 and 4.0 pages): 1.2, 2.2 (the default) and 2.3 run
// Spark 3.5 on Java 17; 3.0 Spark 4.0.1 on Java 21; 4.0 Spark 4.2.0 on
// Java 25.
var (
	spark35Runtime = sparkRuntime{
		image:         "apache/spark@sha256:f3d6eaa8bab8ec2e38f3c3918a5b2f8b253c95bcb19f9e87ad0e0cdacf9df2d5", // 3.5.9-scala2.12-java17-python3-ubuntu
		connectorURL:  "https://repo1.maven.org/maven2/com/google/cloud/spark/spark-3.5-bigquery/0.46.2/spark-3.5-bigquery-0.46.2.jar",
		connectorSHA:  "817aafccd47b4756f7ae5bb51b509d5642140ead0fb3da06f1a20c24e9baa0ac",
		connectorFile: "spark-3.5-bigquery-0.46.2.jar", sparkVersion: "3.5",
	}
	spark40Runtime = sparkRuntime{
		image:         "apache/spark@sha256:fb5c5e61e7bb1be94b7f3a31afe1f73c5b4d20b6008f4ffa7278fc085da08a9e", // 4.0.1-scala2.13-java21-python3-ubuntu
		connectorURL:  "https://repo1.maven.org/maven2/com/google/cloud/spark/spark-4.0-bigquery/0.46.2/spark-4.0-bigquery-0.46.2.jar",
		connectorSHA:  "b09c4455875d7811971796091371d4dda646029b349c35ef95f578f133607e0e",
		connectorFile: "spark-4.0-bigquery-0.46.2.jar", sparkVersion: "4.0",
	}
	spark42Runtime = sparkRuntime{
		image:         "apache/spark@sha256:0a8fa37711b6afe90a1bfe4427a6415b0f315f51f6957ca1bee77d255ebb9e86", // 4.2.0-scala2.13-java25-python3-ubuntu
		connectorURL:  "https://repo1.maven.org/maven2/com/google/cloud/spark/spark-4.2-bigquery/0.46.2/spark-4.2-bigquery-0.46.2.jar",
		connectorSHA:  "45778166a2ebdf1a0e2d758fc0970ff65673f026bc3cb9014dcc242fcba00977",
		connectorFile: "spark-4.2-bigquery-0.46.2.jar", sparkVersion: "4.2",
	}
	sparkRuntimes = map[string]sparkRuntime{"1.2": spark35Runtime, "2.2": spark35Runtime, "2.3": spark35Runtime,
		"3.0": spark40Runtime, "4.0": spark42Runtime}
)

// sparkDefaultRuntime is the runtime a procedure with no runtime_version
// runs on: the default version.
const sparkDefaultRuntime = "2.2"

// sparkRunnerEnabled is whether `up` serves the Spark procedure runner:
// with BigQuery enabled, whose front runs the procedures on it.
func sparkRunnerEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.Spark >= 0
}

// sparkRunnerService serves internal/sparkproc on --port-spark: on
// loopback, and to the BigQuery front through cloudburrow-host.
type sparkRunnerService struct {
	addr string
	srv  *http.Server
	ln   net.Listener
	h    http.Handler
}

// newSparkRunner is the runner, or nil when sparkRunnerEnabled is false.
// live is the instance's bound addresses (BigQuery's, its Storage API's,
// Cloud Logging's and the Artifact Registry's, the Run port), read at each
// run. mint mints the instance's token for a service account, for an
// invoker's @@spark_proc_properties.service_account. keyCheck checks a
// job's customer-managed key on the instance's Cloud KMS (cmekKeys' Usable).
func newSparkRunner(cfg config.Config, out io.Writer, live func() map[string]string, mint func(email string) (string, error),
	keyCheck func(ctx context.Context, project, agent, key string) error) *sparkRunnerService {
	if !sparkRunnerEnabled(cfg) {
		return nil
	}
	cache := filepath.Join(cfg.StateDir, "models", "spark")
	var mu sync.Mutex
	addr := func(name string) func() string {
		return func() string { return live()[name] }
	}
	runtimes := map[string]sparkproc.Runtime{}
	for v, rt := range sparkRuntimes {
		rt := rt
		runtimes[v] = sparkproc.Runtime{Image: rt.image, Connector: func(ctx context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return fetchSparkConnector(ctx, cache, rt, out)
		}}
	}
	d := &sparkproc.Docker{
		Runtimes: runtimes,
		Default:  sparkDefaultRuntime,
		BigQuery: addr("bigquery"),
		Storage:  addr("bigquery-storage"),
		Registry: addr("run"),
		Mint:     mint,
		KeyCheck: keyCheck,
		Instance: cfg.Name,
		Cache:    cache,
		Logf:     func(format string, args ...any) { fmt.Fprintf(out, "cloudburrow: "+format+"\n", args...) },
		Log:      sparkLogger(func() string { return live()["logging"] }),
	}
	return &sparkRunnerService{addr: net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Spark)), h: sparkproc.NewServer(d)}
}

// fetchSparkConnector is rt's connector jar in dir, fetched from Maven
// Central and checked against its pin on the first use.
func fetchSparkConnector(ctx context.Context, dir string, rt sparkRuntime, out io.Writer) (string, error) {
	p := filepath.Join(dir, rt.connectorFile)
	if b, err := os.ReadFile(p); err == nil {
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) == rt.connectorSHA {
			return p, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "cloudburrow: fetching the BigQuery connector for Spark %s (%s)\n", rt.sparkVersion, rt.connectorURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.connectorURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := egress.Client(&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", rt.connectorURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != rt.connectorSHA {
		return "", fmt.Errorf("%s: SHA-256 %x, want the pinned %s", rt.connectorURL, sum, rt.connectorSHA)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// sparkLogger writes each line of a Spark procedure's output to the
// instance's Cloud Logging, when it serves it, as the
// bigquery.googleapis.com/SparkJob resource the page documents: labels
// resource_container (the project), location and spark_job_id, and the
// entry's labels.component DRIVER or EXECUTOR
// (https://docs.cloud.google.com/bigquery/docs/spark-procedures).
func sparkLogger(logging func() string) func(sparkproc.LogEntry) {
	client := egress.Client(&http.Client{Timeout: 10 * time.Second})
	return func(e sparkproc.LogEntry) {
		base := logging()
		if base == "" {
			return
		}
		entry := map[string]any{
			"logName":     "projects/" + e.Job.Project + "/logs/" + sparkLogID,
			"severity":    e.Severity,
			"timestamp":   e.Time.UTC().Format(time.RFC3339Nano),
			"textPayload": e.Text,
			"resource": map[string]any{"type": "bigquery.googleapis.com/SparkJob", "labels": map[string]string{
				"resource_container": e.Job.Project, "location": e.Job.Location, "spark_job_id": e.Job.SparkJobID}},
			"labels": map[string]string{"component": e.Component},
		}
		b, err := json.Marshal(map[string]any{"entries": []any{entry}})
		if err != nil {
			return
		}
		resp, err := client.Post("http://"+base+"/v2/entries:write", "application/json", bytes.NewReader(b))
		if err != nil {
			slog.Warn("spark: a log entry to Cloud Logging failed", "err", err)
			return
		}
		resp.Body.Close()
	}
}

// sparkLogID is the log the entries are written to. UNVERIFIED (docs
// re-read 2026-11-21: https://docs.cloud.google.com/bigquery/docs/spark-procedures
// names the resource and labels to filter by, not the log's ID).
const sparkLogID = "bigquery.googleapis.com%2Fspark"

func (s *sparkRunnerService) Name() string { return "spark-runner" }

func (s *sparkRunnerService) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("spark runner: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	// Loopback-only, but a rebound or cross-site page reaches loopback
	// too (#676); the front names cloudburrow-host, which Wrap accepts.
	s.srv = &http.Server{Handler: hostguard.Wrap(s.h), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *sparkRunnerService) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, "" before Start or for a nil runner.
func (s *sparkRunnerService) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}
