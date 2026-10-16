package logging

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

type fakeDest struct {
	mu      sync.Mutex
	objects map[string]string
	msgs    map[string][]Message
	tables  map[string][]Field
	rows    map[string][]map[string]any
	topics  map[string]bool
}

func newFakeDest() *fakeDest {
	return &fakeDest{objects: map[string]string{}, msgs: map[string][]Message{}, tables: map[string][]Field{},
		rows: map[string][]map[string]any{}, topics: map[string]bool{"projects/proj-one/topics/t": true}}
}

func (f *fakeDest) WriteObject(_ context.Context, bucket, name, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if bucket != "b" {
		return fmt.Errorf("gs://%s: %w", bucket, ErrDestinationNotFound)
	}
	f.objects[name] += string(data)
	return nil
}

func (f *fakeDest) Publish(_ context.Context, topic string, msgs []Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.topics[topic] {
		return fmt.Errorf("%s: %w", topic, ErrDestinationNotFound)
	}
	f.msgs[topic] = append(f.msgs[topic], msgs...)
	return nil
}

func (f *fakeDest) TableSchema(_ context.Context, _, dataset, table string) ([]Field, bool, error) {
	if dataset != "ds" {
		return nil, false, ErrDestinationNotFound
	}
	s, ok := f.tables[table]
	return s, ok, nil
}

func (f *fakeDest) CreateTable(_ context.Context, _, _, table string, fields []Field, _ bool) error {
	f.tables[table] = fields
	return nil
}

func (f *fakeDest) ExtendTable(_ context.Context, _, _, table string, fields []Field) error {
	f.tables[table] = fields
	return nil
}

func (f *fakeDest) InsertRows(_ context.Context, _, _, table string, rows []map[string]any, _ []string) error {
	f.rows[table] = append(f.rows[table], rows...)
	return nil
}

func TestSinkDelivery(t *testing.T) {
	ctx := context.Background()
	st := NewStore(0)
	s := NewServer(st)
	a := NewAdmin(store.NewMemory())
	s.UseAdmin(a)
	f := newFakeDest()
	s.UseDestinations(Destinations{Storage: f, PubSub: f, BigQuery: f})
	base := time.Date(2026, 10, 16, 9, 20, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	cs := a.ConfigServer()
	for id, dest := range map[string]string{
		"gcs": "storage.googleapis.com/b", "ps": "pubsub.googleapis.com/projects/proj-one/topics/t",
		"bq": "bigquery.googleapis.com/projects/proj-one/datasets/ds", "nope": "pubsub.googleapis.com/projects/proj-one/topics/missing",
		"fwd": "logging.googleapis.com/projects/proj-two",
	} {
		sk, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-one", Sink: &loggingpb.LogSink{Name: id, Destination: dest, Filter: `logName="projects/proj-one/logs/app"`}})
		if err != nil {
			t.Fatal(err)
		}
		if sk.GetWriterIdentity() != "serviceAccount:service-proj-one@gcp-sa-logging.iam.gserviceaccount.com" {
			t.Fatalf("writer identity %q", sk.GetWriterIdentity())
		}
	}
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-two", Sink: &loggingpb.LogSink{Name: "p2ps",
		Destination: "pubsub.googleapis.com/projects/proj-one/topics/t", Filter: `severity>=ERROR`}}); err != nil {
		t.Fatal(err)
	}
	payload, _ := structpb.NewStruct(map[string]any{"Msg-Text": "hi", "n": 3, "nested": map[string]any{"ok": true}})
	res := &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "proj-one"}}
	if _, err := s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: []*loggingpb.LogEntry{
		{LogName: "projects/proj-one/logs/app", Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "one"}, Timestamp: timestamppb.New(base)},
		{LogName: "projects/proj-one/logs/app", Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: payload}, Severity: 500, Timestamp: timestamppb.New(base)},
		{LogName: "projects/proj-one/logs/other", Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "unrouted"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s.waitDelivery()

	// Pub/Sub: two from p1's sink, one (ERROR) from p2's, rerouted.
	if got := len(f.msgs["projects/proj-one/topics/t"]); got != 3 {
		t.Fatalf("published %d messages, want 3", got)
	}
	m := f.msgs["projects/proj-one/topics/t"][0]
	if !strings.Contains(string(m.Data), `"textPayload":"one"`) || m.Attributes["logging.googleapis.com/timestamp"] != "2026-10-16T09:20:00Z" {
		t.Fatalf("message %s %v", m.Data, m.Attributes)
	}
	// BigQuery: a date-sharded table, schema from both entries.
	rows := f.rows["app_20261016"]
	if len(rows) != 2 {
		t.Fatalf("rows %v", f.rows)
	}
	jp := rows[1]["jsonPayload"].(map[string]any)
	if jp["msg_text"] != "hi" || jp["n"] != 3.0 || jp["nested"].(map[string]any)["ok"] != true {
		t.Fatalf("jsonPayload row %v", jp)
	}
	// Storage: nothing until the hour ends.
	if len(f.objects) != 0 {
		t.Fatalf("written before the hour ended: %v", f.objects)
	}
	s.Advance(ctx, 40*time.Minute)
	obj := f.objects["app/2026/10/16/09:00:00_09:59:59_S0.json"]
	if strings.Count(obj, "\n") != 2 {
		t.Fatalf("objects %v", f.objects)
	}
	// The project destination: p2's _Default view lists them.
	resp, err := s.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/proj-two"}, Filter: `logName="projects/proj-one/logs/app"`})
	if err != nil || len(resp.GetEntries()) != 2 {
		t.Fatalf("p2 lists %d, %v", len(resp.GetEntries()), err)
	}
	// The missing topic: a sink error entry and the sink's status.
	resp, err = s.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/proj-one"},
		Filter: `resource.type="logging_sink"`})
	if err != nil || len(resp.GetEntries()) != 1 {
		t.Fatalf("sink errors %v, %v", resp.GetEntries(), err)
	}
	e := resp.GetEntries()[0]
	if e.GetLabels()["error_code"] != "topic_not_found" || e.GetLogName() != "projects/proj-one/logs/logging.googleapis.com%2Fsink_error" ||
		e.GetResource().GetLabels()["name"] != "nope" {
		t.Fatalf("sink error %v", e)
	}
	if st := s.SinkStatus("projects/proj-one/sinks/nope"); st == nil || !strings.HasPrefix(st.LastError, "topic_not_found") {
		t.Fatalf("status %+v", st)
	}
	if st := s.SinkStatus("projects/proj-one/sinks/gcs"); st == nil || st.Delivered != 2 || st.Pending != 0 {
		t.Fatalf("gcs status %+v", st)
	}
	// A type change is a schema mismatch: not written, recorded.
	bad, _ := structpb.NewStruct(map[string]any{"n": "three"})
	if _, err := s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: []*loggingpb.LogEntry{
		{LogName: "projects/proj-one/logs/app", Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: bad}, Timestamp: timestamppb.New(base)}}}); err != nil {
		t.Fatal(err)
	}
	s.waitDelivery()
	if len(f.rows["app_20261016"]) != 2 {
		t.Fatal("a mismatched row was written")
	}
	if st := s.SinkStatus("projects/proj-one/sinks/bq"); st == nil || !strings.HasPrefix(st.LastError, "schema_mismatch") {
		t.Fatalf("bq status %+v", st)
	}
}

func TestTableNames(t *testing.T) {
	ts := timestamppb.New(time.Date(2017, 5, 23, 1, 0, 0, 0, time.UTC))
	for log, want := range map[string]string{"syslog": "syslog_20170523", "apache-access": "apache_access_20170523",
		"compute.googleapis.com%2Factivity_log": "compute_googleapis_com_activity_log_20170523"} {
		if got := tableName(&loggingpb.LogEntry{LogName: "projects/proj-x/logs/" + log, Timestamp: ts}, false); got != want {
			t.Errorf("%s: %s, want %s", log, got, want)
		}
	}
	if got := tableName(&loggingpb.LogEntry{LogName: "projects/proj-x/logs/syslog", Timestamp: ts}, true); got != "syslog" {
		t.Errorf("partitioned: %s", got)
	}
}
