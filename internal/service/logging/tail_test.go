package logging

import (
	"context"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestTailLogEntries (#1215): a tail through the official client sees only
// entries written after it opened that match its project and filter, each
// response ordered by timestamp within the buffer window.
func TestTailLogEntries(t *testing.T) {
	t.Parallel()
	_, opts := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeOne := func(log string, ts time.Time) {
		t.Helper()
		_, err := c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName:  "projects/" + project + "/logs/" + log,
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Timestamp: timestamppb.New(ts),
				Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log}}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	writeOne("before", time.Now())

	stream, err := c.TailLogEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&loggingpb.TailLogEntriesRequest{
		ResourceNames: []string{"projects/" + project},
		Filter:        `logName != "projects/` + project + `/logs/skip"`,
		BufferWindow:  durationpb.New(500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	// The server subscribes once it has read the request; give it a moment.
	time.Sleep(200 * time.Millisecond)
	now := time.Now()
	writeOne("late", now)
	writeOne("skip", now)
	writeOne("early", now.Add(-time.Minute))
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range resp.GetEntries() {
		got = append(got, e.GetTextPayload())
	}
	if len(got) != 2 || got[0] != "early" || got[1] != "late" {
		t.Errorf("tail response = %v, want [early late]: in timestamp order, nothing written before the tail, nothing the filter excludes", got)
	}
}

// TestTailLogEntriesRefusals: what the request reference refuses.
func TestTailLogEntriesRefusals(t *testing.T) {
	t.Parallel()
	_, opts := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, req := range []*loggingpb.TailLogEntriesRequest{
		{},
		{ResourceNames: []string{"unknown/1"}},
		{ResourceNames: []string{"projects/" + project}, Filter: `jsonPayload.x = "y"`},
		{ResourceNames: []string{"projects/" + project}, BufferWindow: durationpb.New(61 * time.Second)},
	} {
		stream, err := c.TailLogEntries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(req); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
			t.Errorf("TailLogEntries(%v) = %v, want InvalidArgument", req, err)
		}
	}
}

// TestListMonitoredResourceDescriptors: the global descriptor, as Google
// documents it, through the official client's iterator.
func TestListMonitoredResourceDescriptors(t *testing.T) {
	t.Parallel()
	_, opts := serve(t)
	ctx := context.Background()
	c, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	it := c.ListMonitoredResourceDescriptors(ctx, &loggingpb.ListMonitoredResourceDescriptorsRequest{PageSize: 1})
	var types []string
	for {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, d.GetType())
		if d.GetType() == "global" && (d.GetDisplayName() != "Global" || len(d.GetLabels()) != 1 || d.GetLabels()[0].GetKey() != "project_id") {
			t.Errorf("global descriptor = %v", d)
		}
	}
	if len(types) != 1 || types[0] != "global" {
		t.Errorf("descriptor types = %v, want [global]", types)
	}
}
