//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// loggingAPIClient is the official generated Logging client against the
// instance.
func loggingAPIClient(t *testing.T, h *Harness) *vlogging.Client {
	t.Helper()
	c, err := vlogging.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.logging.v2.LoggingServiceV2/TailLogEntries
//
// TestLoggingTailLogEntries (#1215): a tail opened with the official client,
// a filter and a 500ms buffer window receives the matching entries written
// after it opened, in timestamp order, and not the one written before it or
// the one its filter excludes.
func TestLoggingTailLogEntries(t *testing.T) {
	h := New(t)
	c := loggingAPIClient(t, h)
	ctx, cancel := context.WithTimeout(h.Context(), 30*time.Second)
	defer cancel()
	parent := "projects/" + h.Project()
	write := func(log string, ts time.Time) {
		t.Helper()
		if _, err := c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName: parent + "/logs/" + log, Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Timestamp: timestamppb.New(ts), Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log}}},
		}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	write("tail-before", time.Now())
	stream, err := c.TailLogEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&loggingpb.TailLogEntriesRequest{
		ResourceNames: []string{parent},
		Filter:        `logName != "` + parent + `/logs/tail-skip"`,
		BufferWindow:  durationpb.New(500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	now := time.Now()
	write("tail-late", now)
	write("tail-skip", now)
	write("tail-early", now.Add(-time.Minute))
	var got []string
	for len(got) < 2 {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv after %v: %v", got, err)
		}
		for _, e := range resp.GetEntries() {
			got = append(got, e.GetTextPayload())
		}
	}
	if len(got) != 2 || got[0] != "tail-early" || got[1] != "tail-late" {
		t.Errorf("tailed %v, want [tail-early tail-late]", got)
	}
}

// covers: google.logging.v2.LoggingServiceV2/ListMonitoredResourceDescriptors
//
// TestLoggingListMonitoredResourceDescriptors (#1215): the official
// client's iterator lists the global descriptor, with its project_id label.
func TestLoggingListMonitoredResourceDescriptors(t *testing.T) {
	h := New(t)
	c := loggingAPIClient(t, h)
	it := c.ListMonitoredResourceDescriptors(h.Context(), &loggingpb.ListMonitoredResourceDescriptorsRequest{})
	found := false
	for {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMonitoredResourceDescriptors: %v", err)
		}
		if d.GetType() == "global" {
			found = len(d.GetLabels()) == 1 && d.GetLabels()[0].GetKey() == "project_id"
		}
	}
	if !found {
		t.Error("no global descriptor with a project_id label")
	}
}
