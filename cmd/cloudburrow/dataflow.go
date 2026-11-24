package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/beamrun"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/dataflow"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// dataflowService serves Dataflow (#2215) in the CLI process, gRPC and
// JSON on one port. Its jobs' pipelines run on Apache Flink's Beam job
// server in the host's Docker (internal/beamrun), reaching the instance's
// Cloud Storage, Pub/Sub and BigQuery only; their workers' lines go to its
// Cloud Logging.
type dataflowService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// live is the instance's bound addresses.
	live func() map[string]string
	// registryToken is the token a Flex Template's image is read from
	// the Artifact Registry with; nil reads it with none.
	registryToken func() string
	server        *grpctransport.Server
	api           *dataflow.Server
	// snapTmp is an ephemeral instance's snapshots directory, removed
	// when it stops.
	snapTmp string
	docker  *beamrun.Docker
}

func newDataflowService(cfg config.Config) *dataflowService {
	if !dataflowEnabled(cfg) {
		return nil
	}
	return &dataflowService{cfg: cfg}
}

// dataflowEnabled is whether `up` binds the Dataflow port.
func dataflowEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceDataflow) && cfg.Endpoints.Dataflow >= 0
}

func (s *dataflowService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *dataflowService) Name() string { return "dataflow" }

// API is the server, available after Start.
func (s *dataflowService) API() *dataflow.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *dataflowService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *dataflowService) liveAddrs() map[string]string {
	out := map[string]string{}
	if s.live != nil {
		for k, v := range s.live() {
			out[k] = v
		}
	}
	if a := s.Addr(); a != "" {
		out["dataflow"] = a
	}
	return out
}

func (s *dataflowService) Start(ctx context.Context) error {
	state, snaps := "", ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "dataflow.json")
		snaps = filepath.Join(s.cfg.InstanceDir(), "dataflow-snapshots")
	} else {
		d, err := os.MkdirTemp("", "cb-dataflow-snapshots-")
		if err != nil {
			return err
		}
		snaps, s.snapTmp = d, d
	}
	s.docker = &beamrun.Docker{Instance: s.cfg.Name, Live: s.liveAddrs,
		// A Flex Template's image is read from the instance's Artifact
		// Registry (Cloud Run's) as the instance's account.
		Registry:      func() string { return s.liveAddrs()["run"] },
		RegistryToken: s.registryToken,
		Snapshots:     snaps,
		Logf:          func(format string, args ...any) { fmt.Printf("cloudburrow: "+format+"\n", args...) }}
	opts := dataflow.Options{State: state, Engine: dataflowEngine{d: s.docker}, Launcher: flexLauncher{d: s.docker},
		Logf: func(format string, args ...any) { fmt.Printf("cloudburrow: "+format+"\n", args...) }}
	if s.live != nil && serviceEnabled(s.cfg, config.ServiceStorage) {
		st := &dataprocStorage{live: s.live}
		opts.Storage = st
		// Google-provided templates, built once per state directory.
		opts.Provided = &providedTemplates{p: &beamrun.Provided{D: s.docker, Cache: s.cfg.StateDir}, storage: st,
			project: func() string { return s.cfg.DefaultProject() }}
	}
	if s.live != nil {
		opts.Logs = dataflowLogger{logging: func() string { return s.live()["logging"] }}
	}
	api, err := dataflow.NewServer(opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Dataflow)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = dataflow.NewRESTHandler(api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *dataflowService) Stop(ctx context.Context) error {
	var errs []error
	if s.server != nil {
		errs = append(errs, s.server.Stop(ctx))
	}
	if s.api != nil {
		s.api.Close()
	}
	// What a run's cleanup did not reach (the CLI stopping mid-run).
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	beamrun.RemoveInstance(sctx, s.cfg.Name)
	if s.snapTmp != "" {
		errs = append(errs, os.RemoveAll(s.snapTmp))
	}
	return errors.Join(errs...)
}

// dataflowResetter forgets the jobs, cancelling those that run.
type dataflowResetter struct{ svc *dataflowService }

func (r *dataflowResetter) Name() string { return "dataflow" }

func (r *dataflowResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *dataflowResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the Dataflow API has not started")
	}
	return api.Reset(project)
}

// flexLauncher runs the API's Flex Template launches on beamrun.
type flexLauncher struct{ d *beamrun.Docker }

func (l flexLauncher) Launch(ctx context.Context, f dataflow.FlexLaunch) error {
	return l.d.LaunchFlex(ctx, beamrun.Flex{Project: f.Project, Location: f.Location, JobName: f.JobName, Image: f.Image, Args: f.Args})
}

// dataflowEngine runs the API's jobs on beamrun.
type dataflowEngine struct{ d *beamrun.Docker }

var pythonVersionRE = regexp.MustCompile(`Python (\d+\.\d+)`)

func (e dataflowEngine) Run(ctx context.Context, run dataflow.Run, r dataflow.Reporter) dataflow.Outcome {
	job := beamrun.Job{Project: run.Project, JobID: run.JobID, Name: run.Name, Streaming: run.Streaming,
		PipelineURL: run.PipelineURL, Options: run.Options, SDK: run.SDK, SDKVersion: run.SDKVersion,
		Workers: run.MaxWorkers, Drain: run.Drain, Snapshot: run.SnapshotID}
	if m := pythonVersionRE.FindStringSubmatch(sdkName(run)); m != nil {
		job.PythonVersion = m[1]
	}
	importance := map[string]dataflowpb.JobMessageImportance{
		"JOB_MESSAGE_DEBUG": dataflowpb.JobMessageImportance_JOB_MESSAGE_DEBUG, "JOB_MESSAGE_DETAILED": dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED,
		"JOB_MESSAGE_BASIC": dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "JOB_MESSAGE_WARNING": dataflowpb.JobMessageImportance_JOB_MESSAGE_WARNING,
		"JOB_MESSAGE_ERROR": dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR}
	res := e.d.Run(ctx, job, beamrun.Events{
		Started: r.Started,
		Message: func(imp, text string) {
			i, ok := importance[imp]
			if !ok {
				i = dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED
			}
			r.Message(i, text)
		},
		Graph: func(ts []beamrun.Transform) {
			out := make([]dataflow.Transform, 0, len(ts))
			for _, t := range ts {
				out = append(out, dataflow.Transform{ID: t.ID, Name: t.Name, Kind: t.Kind, Parent: t.Parent, Inputs: t.Inputs, Outputs: t.Outputs, Stage: t.Stage})
			}
			r.Graph(out)
		},
		Metrics: func(ms []beamrun.Metric) {
			now := time.Now()
			out := make([]dataflow.Metric, 0, len(ms))
			for _, m := range ms {
				out = append(out, dataflow.Metric{Name: m.Name, Origin: m.Origin, Step: m.Step, PCollection: m.PCollection,
					Namespace: m.Namespace, Kind: m.Kind, Value: m.Value, Count: m.Count, Min: m.Min, Max: m.Max, Total: m.Total, Tentative: m.Tentative, Updated: now})
			}
			r.Metrics(out)
		},
		Log: r.Log,
	})
	states := map[string]dataflowpb.JobState{"DONE": dataflowpb.JobState_JOB_STATE_DONE, "CANCELLED": dataflowpb.JobState_JOB_STATE_CANCELLED,
		"FAILED": dataflowpb.JobState_JOB_STATE_FAILED}
	st, ok := states[res.State]
	if !ok {
		st = dataflowpb.JobState_JOB_STATE_FAILED
		if res.Message == "" {
			res.Message = "Workflow failed. Causes: the runner ended the job in state " + res.State + "."
		}
	}
	return dataflow.Outcome{State: st, Message: res.Message}
}

// Snapshot takes a Flink savepoint of a running job (beamrun).
func (e dataflowEngine) Snapshot(ctx context.Context, jobID, id string) (int64, error) {
	return e.d.Snapshot(ctx, jobID, id)
}

// DeleteSnapshot removes a snapshot's savepoint.
func (e dataflowEngine) DeleteSnapshot(id string) error { return e.d.DeleteSnapshot(id) }

// sdkName is the job's SDK's name ("Apache Beam Python 3.12 SDK").
func sdkName(run dataflow.Run) string { return run.SDKName }

// dataflowLogger writes a job's worker lines to Cloud Logging as the
// dataflow_step resource, in dataflow.googleapis.com/worker
// (docs.cloud.google.com/dataflow/docs/guides/logging, read 2026-11-24).
type dataflowLogger struct{ logging func() string }

func (l dataflowLogger) Log(project, location, jobID, jobName, severity, text string) {
	base := l.logging()
	if base == "" {
		return
	}
	entry := map[string]any{
		"logName": "projects/" + project + "/logs/dataflow.googleapis.com%2Fworker", "severity": severity,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "textPayload": text,
		"resource": map[string]any{"type": "dataflow_step", "labels": map[string]string{
			"project_id": project, "region": location, "job_id": jobID, "job_name": jobName, "step_id": ""}},
		"labels": map[string]string{"dataflow.googleapis.com/job_id": jobID, "dataflow.googleapis.com/job_name": jobName,
			"dataflow.googleapis.com/region": location},
	}
	b, err := json.Marshal(map[string]any{"entries": []any{entry}})
	if err != nil {
		return
	}
	client := egress.Client(&http.Client{Timeout: 10 * time.Second})
	resp, err := client.Post("http://"+base+"/v2/entries:write", "application/json", bytes.NewReader(b))
	if err != nil {
		slog.Warn("dataflow: a log entry to Cloud Logging failed", "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
