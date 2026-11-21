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

// The Spark runtime of BigQuery's stored procedures for Apache Spark
// (#2104), pinned in dependencies.json (components.bigquerySpark): the
// official apache/spark image of Spark 3.5, the Spark of the Managed
// Service for Apache Spark runtimes 1.2, 2.2 (the default) and 2.3, and
// Google's BigQuery connector for Spark 3.5 from Maven Central.
const (
	sparkImage           = "apache/spark@sha256:f3d6eaa8bab8ec2e38f3c3918a5b2f8b253c95bcb19f9e87ad0e0cdacf9df2d5"
	sparkConnectorURL    = "https://repo1.maven.org/maven2/com/google/cloud/spark/spark-3.5-bigquery/0.46.2/spark-3.5-bigquery-0.46.2.jar"
	sparkConnectorSHA256 = "817aafccd47b4756f7ae5bb51b509d5642140ead0fb3da06f1a20c24e9baa0ac"
	sparkConnectorFile   = "spark-3.5-bigquery-0.46.2.jar"
)

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
// live is the instance's bound addresses (BigQuery's, its Storage API's
// and Cloud Logging's), read at each run.
func newSparkRunner(cfg config.Config, out io.Writer, live func() map[string]string) *sparkRunnerService {
	if !sparkRunnerEnabled(cfg) {
		return nil
	}
	cache := filepath.Join(cfg.StateDir, "models", "spark")
	var mu sync.Mutex
	addr := func(name string) func() string {
		return func() string { return live()[name] }
	}
	d := &sparkproc.Docker{
		Image:    sparkImage,
		BigQuery: addr("bigquery"),
		Storage:  addr("bigquery-storage"),
		Instance: cfg.Name,
		Logf:     func(format string, args ...any) { fmt.Fprintf(out, "cloudburrow: "+format+"\n", args...) },
		Log:      sparkLogger(func() string { return live()["logging"] }),
		Connector: func(ctx context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return fetchSparkConnector(ctx, cache, out)
		},
	}
	return &sparkRunnerService{addr: net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Spark)), h: sparkproc.NewServer(d)}
}

// fetchSparkConnector is the connector jar in dir, fetched from Maven
// Central and checked against its pin on the first use.
func fetchSparkConnector(ctx context.Context, dir string, out io.Writer) (string, error) {
	p := filepath.Join(dir, sparkConnectorFile)
	if b, err := os.ReadFile(p); err == nil {
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) == sparkConnectorSHA256 {
			return p, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "cloudburrow: fetching the BigQuery connector for Spark (%s)\n", sparkConnectorURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sparkConnectorURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := egress.Client(&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", sparkConnectorURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != sparkConnectorSHA256 {
		return "", fmt.Errorf("%s: SHA-256 %x, want the pinned %s", sparkConnectorURL, sum, sparkConnectorSHA256)
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
