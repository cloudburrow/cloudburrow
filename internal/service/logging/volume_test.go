package logging

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestSinkVolumes (#2021): each sink counts the entries and bytes it
// routed (a Pub/Sub sink once the topic took them, a project sink at
// once) and the entries it failed to deliver; a dry run counts nothing;
// ingestion counts each entry once per project bucket that stores it.
func TestSinkVolumes(t *testing.T) {
	ctx := context.Background()
	s := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	s.UseAdmin(a)
	f := newFakeDest()
	s.UseDestinations(Destinations{PubSub: f})
	base := time.Date(2026, 11, 20, 9, 20, 30, 0, time.UTC)
	s.now = func() time.Time { return base }
	for id, dest := range map[string]string{"ps": "pubsub.googleapis.com/projects/proj-one/topics/t",
		"nope": "pubsub.googleapis.com/projects/proj-one/topics/missing", "fwd": "logging.googleapis.com/projects/proj-two"} {
		if _, err := a.ConfigServer().CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-one",
			Sink: &loggingpb.LogSink{Name: id, Destination: dest}}); err != nil {
			t.Fatal(err)
		}
	}
	res := &mrpb.MonitoredResource{Type: "global"}
	es := []*loggingpb.LogEntry{
		{LogName: "projects/proj-one/logs/app", Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "one"}, Timestamp: timestamppb.New(base)},
		{LogName: "projects/proj-one/logs/app", Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "two"}, Timestamp: timestamppb.New(base)},
	}
	if _, err := s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: es, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if got := s.SinkVolumes("projects/proj-one", base.Add(-time.Hour), base.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("a dry run counted %+v", got)
	}
	if _, err := s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: res, Entries: es}); err != nil {
		t.Fatal(err)
	}
	s.waitDelivery()
	by := map[string]SinkVolume{}
	for _, v := range s.SinkVolumes("projects/proj-one", base.Add(-time.Hour), base.Add(time.Hour)) {
		by[v.Sink] = v
		if !v.End.Equal(base.Truncate(time.Minute).Add(time.Minute)) {
			t.Errorf("%s counted in the minute ending %v", v.Sink, v.End)
		}
	}
	// The entries as stored, with the resource, insert ID and receive
	// time the write gave them.
	var size int64
	for _, e := range s.ProjectEntries("proj-one", base.Add(-time.Hour), base.Add(time.Hour)) {
		if e.GetLogName() == "projects/proj-one/logs/app" {
			size += int64(proto.Size(e))
		}
	}
	if v := by["projects/proj-one/sinks/ps"]; v.Entries != 2 || v.Bytes != size || v.Errors != 0 {
		t.Errorf("the Pub/Sub sink = %+v; want 2 entries of %d bytes", v, size)
	}
	if v := by["projects/proj-one/sinks/nope"]; v.Entries != 0 || v.Errors != 2 {
		t.Errorf("the failing sink = %+v; want 2 errors", v)
	}
	if v := by["projects/proj-one/sinks/fwd"]; v.Entries != 2 || v.Destination != "logging.googleapis.com/projects/proj-two" {
		t.Errorf("the project sink = %+v", v)
	}
	sum := func(p string) int64 {
		var n int64
		for _, i := range s.Ingested(p, base.Add(-time.Hour), base.Add(time.Hour)) {
			n += i.Bytes
		}
		return n
	}
	if sum("proj-one") != size || sum("proj-two") != size {
		t.Errorf("ingested proj-one %d, proj-two %d; want %d each", sum("proj-one"), sum("proj-two"), size)
	}
}
