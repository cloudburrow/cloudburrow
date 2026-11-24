// Package dataflow serves the Dataflow API (dataflow.googleapis.com
// v1b3, #2215): jobs (create, get, list, aggregated, update for cancel,
// drain and the runtime-updatable options, and pipeline update through
// replaceJobId), their messages, metrics and execution details, classic
// templates (templates.create, launch and get from a template file in
// Cloud Storage) and Flex Templates.
//
// A job runs on an Engine: CloudBurrow's is internal/beamrun, the job's
// staged Apache Beam pipeline on Apache Flink's Beam job server with the
// SDK's own harness, in containers on a Docker network whose one route is
// a relay to the instance's Cloud Storage, Pub/Sub and BigQuery. The SDKs'
// DataflowRunner submits through this API as it does to Google's: it
// stages the pipeline (pipeline.pb) and its files in the instance's Cloud
// Storage and creates the job, whose environment.sdkPipelineOptions name
// them. Without an engine, a job that would run fails, with its reason as
// its error message.
//
// As Google documents it (docs.cloud.google.com/dataflow/docs: the v1b3
// REST reference for projects.locations.jobs, .messages, .templates and
// .flexTemplates, guides/stopping-a-pipeline, guides/updating-a-pipeline,
// concepts/regional-endpoints, read 2026-11-24):
//
//   - a job's name matches [a-z]([-a-z0-9]{0,1022}[a-z0-9])?, and one
//     active job of a name runs in a project's region at a time;
//   - projects.jobs.* (no location) is us-central1;
//   - a job moves PENDING, RUNNING, then DONE or FAILED; requestedState
//     CANCELLED moves it through CANCELLING to CANCELLED, DRAINED (a
//     streaming job's) through DRAINING to DRAINED; a replacement job
//     (replaceJobId) leaves the job it replaces UPDATED;
//   - JOB_VIEW_SUMMARY is the default view of get and list.
//
// Error texts Google's pages do not quote are marked UNVERIFIED where they
// are made (docs re-read 2026-11-24: docs.cloud.google.com/dataflow/docs/reference/rest).
package dataflow

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultLocation is the region of projects.jobs.* (no location), as
// Google documents.
const DefaultLocation = "us-central1"

// Storage reads a template's files from the instance's Cloud Storage.
type Storage interface {
	Read(ctx context.Context, uri string) ([]byte, error)
}

// Logger writes a job's worker lines to the instance's Cloud Logging, as
// the dataflow_step resource.
type Logger interface {
	Log(project, location, jobID, jobName, severity, text string)
}

// Launcher runs a Flex Template's launcher image (#2215): the template's
// container, on the job's network, whose program submits the job through
// this API with the SDK's DataflowRunner.
type Launcher interface {
	Launch(ctx context.Context, l FlexLaunch) error
}

// FlexLaunch is one Flex Template launch.
type FlexLaunch struct {
	Project, Location, JobName string
	// Image is the template's container image (an Artifact Registry
	// image), Env its environment for the launcher (FLEX_TEMPLATE_*).
	Image string
	// Args are the pipeline's arguments: the template's parameters and
	// the runner's options.
	Args []string
}

// Options are the server's collaborators.
type Options struct {
	// State is the JSON file the server persists to; "" keeps nothing.
	State string
	// Engine runs the pipelines; nil fails each job that would run.
	Engine Engine
	// Storage reads templates; nil refuses them.
	Storage Storage
	// Launcher runs Flex Templates; nil refuses them.
	Launcher Launcher
	// Logs receives the workers' lines; nil drops them.
	Logs Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logf receives the server's own notes; nil drops them.
	Logf func(format string, args ...any)
}

// Server is the Dataflow API.
type Server struct {
	dataflowpb.UnimplementedJobsV1Beta3Server
	dataflowpb.UnimplementedMessagesV1Beta3Server
	dataflowpb.UnimplementedMetricsV1Beta3Server
	dataflowpb.UnimplementedSnapshotsV1Beta3Server
	dataflowpb.UnimplementedTemplatesServiceServer
	dataflowpb.UnimplementedFlexTemplatesServiceServer

	opts Options
	mu   sync.Mutex
	// jobs by key (project/location/id).
	jobs map[string]*jobRec
	wg   sync.WaitGroup
}

// jobRec is one job and what it reported.
type jobRec struct {
	job      *dataflowpb.Job
	messages []*dataflowpb.JobMessage
	scaling  []*dataflowpb.AutoscalingEvent
	metrics  []Metric
	// cancel stops the run; drain is closed to drain it; done is closed
	// when the run ends. All nil once it has.
	cancel  context.CancelFunc
	drain   chan struct{}
	drained bool
	done    chan struct{}
	// replacement is the job to start when this one has stopped for an
	// update (replaceJobId).
	replacement string
	msgSeq      int
	// flex is a Flex Template's job, QUEUED until its launcher's program
	// submits the pipeline under its name.
	flex bool
	// flexTemp is a Flex Template launch's temporary location, which its
	// job's options carry as tempLocation (the SDK's temp_location is
	// that location with the job's name and time appended).
	flexTemp string
}

// NewServer returns the server, loading opts.State.
func NewServer(opts Options) (*Server, error) {
	s := &Server{opts: opts, jobs: map[string]*jobRec{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

func jobKey(project, location, id string) string { return project + "/" + location + "/" + id }

// pacific is the zone a job ID's time is in, as Google's are
// (2026-11-24_06_55_12-…: US Pacific).
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.UTC
}()

// newJobID is a job ID in Google's form: the create time
// (YYYY-MM-DD_HH_MM_SS) and a random number.
func newJobID(t time.Time) string {
	n, _ := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil))
	return t.In(pacific).Format("2006-01-02_15_04_05") + "-" + n.String()
}

// Close cancels every running job and waits for their runs to end.
func (s *Server) Close() {
	s.mu.Lock()
	for _, r := range s.jobs {
		if r.cancel != nil {
			r.cancel()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Reset forgets the jobs of project ("" every project), cancelling those
// that run.
func (s *Server) Reset(project string) error {
	s.mu.Lock()
	var wait []chan struct{}
	for k, r := range s.jobs {
		if project != "" && r.job.GetProjectId() != project {
			continue
		}
		if r.cancel != nil {
			r.cancel()
			wait = append(wait, r.done)
		}
		delete(s.jobs, k)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	for _, d := range wait {
		<-d
	}
	return err
}

// Jobs are the project's jobs ("" every project's), newest first, in the
// full view: the console's list.
func (s *Server) Jobs(project string) []*dataflowpb.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*dataflowpb.Job
	for _, r := range s.jobs {
		if project == "" || r.job.GetProjectId() == project {
			out = append(out, proto.Clone(r.job).(*dataflowpb.Job))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].GetCreateTime().AsTime(), out[j].GetCreateTime().AsTime()
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].GetId() > out[j].GetId()
	})
	return out
}

type persistedJob struct {
	Job      json.RawMessage   `json:"job"`
	Messages []json.RawMessage `json:"messages,omitempty"`
	Scaling  []json.RawMessage `json:"scaling,omitempty"`
	Metrics  []Metric          `json:"metrics,omitempty"`
}

type persisted struct {
	Jobs []persistedJob `json:"jobs"`
}

func (s *Server) load() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("read %s: %w", s.opts.State, err)
	}
	now := timestamppb.New(s.now())
	for _, pj := range p.Jobs {
		var j dataflowpb.Job
		if err := protojson.Unmarshal(pj.Job, &j); err != nil {
			continue
		}
		r := &jobRec{job: &j, metrics: pj.Metrics}
		for _, raw := range pj.Messages {
			var m dataflowpb.JobMessage
			if protojson.Unmarshal(raw, &m) == nil {
				r.messages = append(r.messages, &m)
			}
		}
		for _, raw := range pj.Scaling {
			var e dataflowpb.AutoscalingEvent
			if protojson.Unmarshal(raw, &e) == nil {
				r.scaling = append(r.scaling, &e)
			}
		}
		r.msgSeq = len(r.messages)
		if !terminal(j.CurrentState) {
			// Its workers were the containers the last run of the CLI
			// removed when it stopped.
			j.CurrentState, j.CurrentStateTime = dataflowpb.JobState_JOB_STATE_FAILED, now
			s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR,
				"Workflow failed. Causes: The job's workers stopped when the CloudBurrow instance stopped.")
		}
		s.jobs[jobKey(j.ProjectId, j.Location, j.Id)] = r
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	var p persisted
	keys := make([]string, 0, len(s.jobs))
	for k := range s.jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := protojson.MarshalOptions{}
	for _, k := range keys {
		r := s.jobs[k]
		jb, _ := m.Marshal(r.job)
		pj := persistedJob{Job: jb, Metrics: r.metrics}
		for _, x := range r.messages {
			b, _ := m.Marshal(x)
			pj.Messages = append(pj.Messages, b)
		}
		for _, x := range r.scaling {
			b, _ := m.Marshal(x)
			pj.Scaling = append(pj.Scaling, b)
		}
		p.Jobs = append(p.Jobs, pj)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o755); err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

func (s *Server) save() {
	if err := s.saveLocked(); err != nil {
		s.logf("dataflow: save state: %v", err)
	}
}

// addMessageLocked appends a job message.
func (s *Server) addMessageLocked(r *jobRec, imp dataflowpb.JobMessageImportance, text string) {
	r.msgSeq++
	r.messages = append(r.messages, &dataflowpb.JobMessage{
		Id:   fmt.Sprintf("%d", s.now().UnixNano()/1000+int64(r.msgSeq)),
		Time: timestamppb.New(s.now()), MessageText: text, MessageImportance: imp})
}

func terminal(st dataflowpb.JobState) bool {
	switch st {
	case dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED,
		dataflowpb.JobState_JOB_STATE_UPDATED, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_STOPPED:
		return true
	}
	return false
}

// active is whether a job in st counts as active: listed by the ACTIVE
// filter and holding its name.
func active(st dataflowpb.JobState) bool {
	return st != dataflowpb.JobState_JOB_STATE_UNKNOWN && !terminal(st)
}
