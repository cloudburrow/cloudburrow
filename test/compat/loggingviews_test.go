//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// loggingViewFixture makes, through the official ConfigClient, a bucket with
// a one-day retention, a sink routing log logID to it, and a view on it whose
// filter keeps logID's gce_instance entries; it writes, through the official
// Client, a gce_instance and a global entry now and a gce_instance entry two
// days old. It returns the bucket and the view.
func loggingViewFixture(t *testing.T, h *Harness, logID string) (bucket, view string) {
	t.Helper()
	ctx := h.Context()
	cc, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	project := "projects/" + h.Project()
	id := strings.ReplaceAll(logID, "-", "_")
	b, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: project + "/locations/global", BucketId: id,
		Bucket: &loggingpb.LogBucket{RetentionDays: 1}})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	sink, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: project, Sink: &loggingpb.LogSink{Name: id,
		Destination: "logging.googleapis.com/" + b.GetName(), Filter: fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	v, err := cc.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: b.GetName(), ViewId: "gce",
		View: &loggingpb.LogView{Filter: fmt.Sprintf(`SOURCE("%s") AND LOG_ID("%s") AND resource.type = "gce_instance"`, project, logID)}})
	if err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: sink.GetName()})
		_ = cc.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: v.GetName()})
		_ = cc.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: b.GetName()})
	})
	lc := loggingAPIClient(t, h)
	now := time.Now()
	for _, e := range []struct {
		typ, text string
		ts        time.Time
	}{{"gce_instance", "gce-now", now}, {"global", "global-now", now}, {"gce_instance", "gce-old", now.Add(-48 * time.Hour)}} {
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: project + "/logs/" + logID,
			Resource: &mrpb.MonitoredResource{Type: e.typ, Labels: map[string]string{}},
			Entries:  []*loggingpb.LogEntry{{Timestamp: timestamppb.New(e.ts), Payload: &loggingpb.LogEntry_TextPayload{TextPayload: e.text}}}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	return b.GetName(), v.GetName()
}

// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
// covers: google.logging.v2.LoggingServiceV2/TailLogEntries
// covers: google.logging.v2.ConfigServiceV2/CreateView
//
// TestLoggingViewFiltersAndRetention (#1410): through the official apiv2
// Client, ListLogEntries with resource_names a bucket view returns the
// bucket's entries its filter (SOURCE(), LOG_ID(), resource.type) matches,
// _AllLogs returns them all, and either leaves out the entries older than
// the bucket's retention_days, while the project's _Default (30 days) still
// lists them. TailLogEntries on the view streams only what the view matches.
func TestLoggingViewFiltersAndRetention(t *testing.T) {
	h := New(t)
	logID := fmt.Sprint("views-", time.Now().UnixNano())
	bucket, view := loggingViewFixture(t, h, logID)
	lc := loggingAPIClient(t, h)
	project := "projects/" + h.Project()
	logFilter := fmt.Sprintf(`logName="%s/logs/%s"`, project, logID)
	read := func(name string) []string {
		got := entryTextsFiltered(t, h, lc, logFilter, name)
		sort.Strings(got)
		return got
	}
	for name, want := range map[string]string{
		bucket + "/views/_AllLogs": "gce-now,global-now",
		view:                       "gce-now",
		project:                    "gce-now,gce-old,global-now",
	} {
		if got := strings.Join(read(name), ","); got != want {
			t.Errorf("ListLogEntries(%s) = %s; want %s", name, got, want)
		}
	}

	ctx, cancel := context.WithTimeout(h.Context(), 30*time.Second)
	defer cancel()
	stream, err := lc.TailLogEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&loggingpb.TailLogEntriesRequest{ResourceNames: []string{view}, BufferWindow: durationpb.New(0)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	for _, typ := range []string{"global", "gce_instance"} {
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: project + "/logs/" + logID,
			Resource: &mrpb.MonitoredResource{Type: typ},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "tail-" + typ}}}}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(resp.GetEntries()) != 1 || resp.GetEntries()[0].GetTextPayload() != "tail-gce_instance" {
		t.Errorf("the tail on the view sent %v; want only tail-gce_instance", resp.GetEntries())
	}
}

// TestGcloudLoggingReadView (#1410): `gcloud logging read --bucket --view
// --location` reads a view through its filter and its bucket's retention,
// through the harness (egress blocked).
func TestGcloudLoggingReadView(t *testing.T) {
	h := New(t)
	logID := fmt.Sprint("gviews-", time.Now().UnixNano())
	bucket, _ := loggingViewFixture(t, h, logID)
	bucketID := bucket[strings.LastIndex(bucket, "/")+1:]
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	logFilter := fmt.Sprintf(`logName="projects/%s/logs/%s"`, h.Project(), logID)
	for view, want := range map[string]string{"gce": "gce-now", "_AllLogs": "gce-now,global-now"} {
		out := g.must("logging", "read", logFilter, "--bucket="+bucketID, "--view="+view, "--location=global", p, "--format=value(textPayload)")
		got := strings.Fields(out)
		sort.Strings(got)
		if strings.Join(got, ",") != want {
			t.Errorf("gcloud logging read --view=%s = %q; want %s", view, out, want)
		}
	}
}

// entryTextsFiltered lists the text payloads of names' entries filter
// matches.
func entryTextsFiltered(t *testing.T, h *Harness, c *vlogging.Client, filter string, names ...string) []string {
	t.Helper()
	var out []string
	it := c.ListLogEntries(h.Context(), &loggingpb.ListLogEntriesRequest{ResourceNames: names, Filter: filter})
	for {
		e, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListLogEntries(%v): %v", names, err)
		}
		out = append(out, e.GetTextPayload())
	}
}
