package dataflow

import (
	"context"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
)

// Engine runs a job's Apache Beam pipeline. CloudBurrow's is
// internal/beamrun: Apache Flink's Beam job server and the job's SDK
// harness in the host's Docker, on a network whose one route is a relay to
// the instance's own APIs.
type Engine interface {
	// Run runs the job to its end, reporting what it does through r, and
	// returns how it ended. Cancelling ctx cancels the job.
	Run(ctx context.Context, run Run, r Reporter) Outcome
}

// Run is one job as the engine runs it.
type Run struct {
	// Project, Location, JobID and Name identify the job.
	Project, Location, JobID, Name string
	// Streaming is a JOB_TYPE_STREAMING job.
	Streaming bool
	// PipelineURL is the staged Beam pipeline (gs://…/pipeline.pb), the
	// SDK's proto_pipeline_staged_url (Python's options.pipelineUrl).
	PipelineURL string
	// Options are the pipeline options the SDK sent
	// (environment.sdkPipelineOptions.options), as JSON values.
	Options map[string]any
	// SDK is the SDK's language ("Python", "Java", "Go"), from the job's
	// user agent; SDKVersion its Beam version.
	SDK, SDKVersion string
	// SDKName is the user agent's name ("Apache Beam Python 3.12 SDK").
	SDKName string
	// MaxWorkers bounds the job's workers (the parallelism the runner is
	// given): environment.workerPools[0].autoscalingSettings.maxNumWorkers,
	// or numWorkers.
	MaxWorkers int
	// Drain is closed when the job is to drain: its sources stop reading
	// and what they read is processed and written.
	Drain <-chan struct{}
	// SnapshotID is the snapshot the job is created from
	// (createdFromSnapshotId): its state is restored from it.
	SnapshotID string
}

// Outcome is how a run ended.
type Outcome struct {
	// State is JOB_STATE_DONE, _FAILED, _CANCELLED or _DRAINED.
	State dataflowpb.JobState
	// Message is the failure's cause, the job's last error message.
	Message string
}

// Reporter receives what a running job does.
type Reporter interface {
	// Started is called when the pipeline is running on its workers, with
	// the number of workers it runs on.
	Started(workers int)
	// Message adds a job message.
	Message(importance dataflowpb.JobMessageImportance, text string)
	// Metrics replaces the job's metrics: each step's element counts and
	// the pipeline's own counters.
	Metrics(m []Metric)
	// Log is one line of a worker's log, for Cloud Logging.
	Log(severity, text string)
	// Graph is the pipeline's transforms, as the runner reads them from
	// the staged pipeline: the job graph.
	Graph(ts []Transform)
}

// Transform is one transform of a job's pipeline.
type Transform struct {
	// ID is its ID in the pipeline (the step name metrics name); Name its
	// user name (unique_name); Kind what it does (ParDo, GroupByKey,
	// Read, ...).
	ID, Name, Kind string
	// Parent is the composite it is part of ("" a top-level one).
	Parent string
	// Inputs and Outputs are the PCollections it reads and writes.
	Inputs, Outputs []string
	// Stage is the fused stage it runs in.
	Stage string
}

// Metric is one metric of a running job.
type Metric struct {
	// Name is the metric's name; Origin "dataflow/v1b3" for the service's
	// own (ElementCount) or "user" for the pipeline's.
	Name, Origin string
	// Step is the job step (transform) it was counted in; PCollection the
	// output it counts (ElementCount's).
	Step, PCollection string
	// Namespace is a user metric's namespace.
	Namespace string
	// Kind is "Sum", "Distribution" or "Latest"; Value a Sum's or
	// Latest's value; Count, Min, Max and Total a Distribution's.
	Kind                   string
	Value                  int64
	Count, Min, Max, Total int64
	Updated                time.Time
}
