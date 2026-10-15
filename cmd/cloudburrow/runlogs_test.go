package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	ltype "google.golang.org/genproto/googleapis/logging/type"
)

// TestRunLogTargets (#1298): a revision's pod is a cloud_run_revision source
// for its user container only, a job task's pod a cloud_run_job source with
// the execution, task index and attempt labels; a pod of an object the
// adapter did not make is not read.
func TestRunLogTargets(t *testing.T) {
	raw := `{"items":[
 {"kind":"Service","metadata":{"name":"hello","labels":{"cloudburrow.dev/owned":"true"},
   "annotations":{"cloudburrow.dev/cloud-run-name":"projects/proj-1/locations/us-central1/services/hello"}}},
 {"kind":"Job","metadata":{"name":"exec-1","labels":{"cloudburrow.dev/owned":"true"},
   "annotations":{"cloudburrow.dev/cloud-run-name":"projects/proj-1/locations/europe-west1/jobs/batch/executions/exec-1"}}},
 {"kind":"Pod","metadata":{"name":"hello-00001-deployment-x","uid":"u1","labels":{"serving.knative.dev/service":"hello",
   "serving.knative.dev/revision":"hello-00001","serving.knative.dev/configuration":"hello"}},
  "spec":{"containers":[{"name":"user-container"},{"name":"queue-proxy"}]},"status":{"phase":"Running"}},
 {"kind":"Pod","metadata":{"name":"exec-1-0-abc","uid":"u2","labels":{"batch.kubernetes.io/job-name":"exec-1"},
   "annotations":{"batch.kubernetes.io/job-completion-index":"2"}},
  "spec":{"containers":[{"name":"main"}]},"status":{"phase":"Succeeded"}},
 {"kind":"Pod","metadata":{"name":"other","uid":"u3","labels":{"serving.knative.dev/service":"not-ours"}},
  "spec":{"containers":[{"name":"user-container"}]},"status":{"phase":"Running"}}
]}`
	srcs, err := runLogTargets([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %+v; want the revision's user container and the task's", srcs)
	}
	rev, task := srcs[0], srcs[1]
	if rev.container != "user-container" || rev.resource.GetType() != "cloud_run_revision" || rev.project != "proj-1" ||
		rev.resource.GetLabels()["service_name"] != "hello" || rev.resource.GetLabels()["revision_name"] != "hello-00001" ||
		rev.resource.GetLabels()["configuration_name"] != "hello" || rev.resource.GetLabels()["location"] != "us-central1" ||
		rev.labels["instanceId"] != "u1" || rev.terminal {
		t.Errorf("revision source = %+v %v", rev, rev.resource)
	}
	if task.resource.GetType() != "cloud_run_job" || task.resource.GetLabels()["job_name"] != "batch" ||
		task.resource.GetLabels()["location"] != "europe-west1" || task.labels["run.googleapis.com/execution_name"] != "exec-1" ||
		task.labels["run.googleapis.com/task_index"] != "2" || task.labels["run.googleapis.com/task_attempt"] != "0" || !task.terminal {
		t.Errorf("task source = %+v %v", task, task.resource)
	}
}

// TestRunLogEntry (#1298): a plain line is a textPayload with severity
// DEFAULT; a JSON object line is a jsonPayload whose severity and
// logging.googleapis.com fields are read out of it, the message kept.
func TestRunLogEntry(t *testing.T) {
	srcs, _ := runLogTargets([]byte(`{"items":[
 {"kind":"Service","metadata":{"name":"s","labels":{"cloudburrow.dev/owned":"true"},
   "annotations":{"cloudburrow.dev/cloud-run-name":"projects/proj-1/locations/us-central1/services/s"}}},
 {"kind":"Pod","metadata":{"name":"p","uid":"u","labels":{"serving.knative.dev/service":"s"}},
  "spec":{"containers":[{"name":"c"}]},"status":{"phase":"Running"}}]}`))
	ts := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e := runLogEntry(srcs[0], runStdout, ts, "hello world")
	if e.GetLogName() != "projects/proj-1/logs/run.googleapis.com%2Fstdout" || e.GetTextPayload() != "hello world" ||
		e.GetSeverity() != ltype.LogSeverity_DEFAULT || !e.GetTimestamp().AsTime().Equal(ts) {
		t.Errorf("plain entry = %v", e)
	}
	e = runLogEntry(srcs[0], runStderr, ts, `{"severity":"error","message":"boom","logging.googleapis.com/labels":{"k":"v"},"logging.googleapis.com/trace":"projects/proj-1/traces/abc","n":1}`)
	p := e.GetJsonPayload().GetFields()
	if e.GetLogName() != "projects/proj-1/logs/run.googleapis.com%2Fstderr" || e.GetSeverity() != ltype.LogSeverity_ERROR ||
		p["message"].GetStringValue() != "boom" || p["n"].GetNumberValue() != 1 || p["severity"] != nil ||
		e.GetLabels()["k"] != "v" || e.GetLabels()["instanceId"] != "u" || e.GetTrace() != "projects/proj-1/traces/abc" {
		t.Errorf("JSON entry = %v", e)
	}
	if e := runLogEntry(srcs[0], runStdout, ts, `[1,2]`); e.GetTextPayload() != "[1,2]" {
		t.Errorf("a JSON array = %v; want a textPayload", e)
	}
}

// TestRunLogFeederReadsCRIStreams (#1298): the runtime's log file names each
// line's stream, so a stdout line goes to run.googleapis.com/stdout and a
// stderr line to run.googleapis.com/stderr; a line the runtime split (P) is
// joined to its continuation, and a line older than since is not written.
func TestRunLogFeederReadsCRIStreams(t *testing.T) {
	var got []*loggingpb.LogEntry
	f := &runLogFeeder{write: func(_ context.Context, r *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
		got = append(got, r.GetEntries()...)
		return &loggingpb.WriteLogEntriesResponse{}, nil
	}}
	src := runLogSource{project: "proj-1", labels: map[string]string{}}
	in := "2026-10-15T11:00:00.000000001Z stdout F too old\n" +
		"2026-10-15T12:00:00.000000001Z stdout F plain\n" +
		"2026-10-15T12:00:00.000000002Z stderr P {\"severity\":\n" +
		"2026-10-15T12:00:00.000000003Z stderr F \"ERROR\",\"message\":\"m\"}\n" +
		"not a CRI line\n"
	last, n := f.read(context.Background(), strings.NewReader(in), src, time.Date(2026, 10, 15, 11, 30, 0, 0, time.UTC))
	if n != int64(len(in)) || last.Nanosecond() != 3 {
		t.Errorf("read = %v, %d; want the last line's time and %d bytes", last, n, len(in))
	}
	if len(got) != 2 || got[0].GetLogName() != "projects/proj-1/logs/run.googleapis.com%2Fstdout" || got[0].GetTextPayload() != "plain" ||
		got[1].GetLogName() != "projects/proj-1/logs/run.googleapis.com%2Fstderr" || got[1].GetSeverity() != ltype.LogSeverity_ERROR ||
		got[1].GetJsonPayload().GetFields()["message"].GetStringValue() != "m" {
		t.Errorf("entries = %v", got)
	}
}
