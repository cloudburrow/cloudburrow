package logging

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A view's filter and a bucket's retention apply when it is read (#1410).
func TestViewFilterAndRetentionOnRead(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	cs := a.ConfigServer()
	const g = "projects/proj-1/locations/global"
	for _, v := range []*loggingpb.LogView{
		{Name: "stdout", Filter: `SOURCE("projects/proj-1") AND LOG_ID("stdout")`},
		{Name: "notgce", Filter: `NOT resource.type = "gce_instance"`},
		{Name: "or", Filter: `resource.type = "a" OR resource.type = "b"`},
	} {
		if _, err := cs.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: g + "/buckets/_Default", ViewId: v.Name,
			View: &loggingpb.LogView{Filter: v.Filter}}); err != nil {
			t.Fatalf("CreateView %s: %v", v.Name, err)
		}
	}
	now := time.Now()
	write := func(log, typ string, ts time.Time) {
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
			LogName: "projects/proj-1/logs/" + log, Resource: &monitoredres.MonitoredResource{Type: typ}, Timestamp: timestamppb.New(ts),
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log + "/" + typ}}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("stdout", "gce_instance", now)
	write("stderr", "global", now)
	write("stdout", "global", now.Add(-48*time.Hour))
	read := func(name string) ([]string, codes.Code) {
		resp, err := srv.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{name}})
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetTextPayload())
		}
		return out, status.Code(err)
	}
	for name, want := range map[string]int{
		"projects/proj-1": 3, g + "/buckets/_Default/views/_AllLogs": 3,
		g + "/buckets/_Default/views/stdout": 2, g + "/buckets/_Default/views/notgce": 2,
	} {
		if got, code := read(name); len(got) != want || code != codes.OK {
			t.Errorf("read %s = %v, %v; want %d entries", name, got, code, want)
		}
	}
	if _, code := read(g + "/buckets/_Default/views/or"); code != codes.Unimplemented {
		t.Errorf("a view with OR = %v, want UNIMPLEMENTED", code)
	}

	// Retention: one day on _Default ages the two-day-old entry out, and
	// advancing the clock ages the others out too.
	if _, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: g + "/buckets/_Default",
		Bucket: &loggingpb.LogBucket{RetentionDays: 1}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := read("projects/proj-1"); len(got) != 2 {
		t.Errorf("after a 1-day retention: %v, want the 2 recent entries", got)
	}
	srv.Advance(ctx, 25*time.Hour)
	if got, _ := read(g + "/buckets/_Default/views/stdout"); len(got) != 0 {
		t.Errorf("after the clock passed the retention: %v, want none", got)
	}
}
