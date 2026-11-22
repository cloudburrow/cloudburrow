package logging

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestLogMetricsAndLogMatchFollowTheLoggingClock (main run 45023269046):
// once clock:advance has moved the Logging clock ahead, which entries'
// receiveTimestamp is stamped by, a log-based metric still counts only the
// entries received after it was made (TestMonitoringLogBasedMetrics
// counted the one written before), and a log-match condition's window, in
// Cloud Monitoring's wall-clock times, still holds the entries received in
// it (TestMonitoringLogMatchAlert opened no incident).
func TestLogMetricsAndLogMatchFollowTheLoggingClock(t *testing.T) {
	ctx := context.Background()
	wall := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	now := func() time.Time { return wall }
	a := NewAdmin(store.NewMemory())
	a.now = now
	srv := NewServer(NewStore(100))
	srv.now = now
	srv.UseAdmin(a)
	srv.Advance(ctx, time.Hour)

	write := func(msg string) {
		t.Helper()
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/checkout",
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: msg}}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("before")
	wall = wall.Add(time.Second)
	if _, err := a.MetricsServer().CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "projects/proj-1",
		Metric: &loggingpb.LogMetric{Name: "errs", Filter: `logName = "projects/proj-1/logs/checkout"`}}); err != nil {
		t.Fatal(err)
	}
	wall = wall.Add(time.Second)
	write("after")

	got, err := srv.MetricSamples("proj-1", "errs", wall.Add(-time.Hour), wall.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Entry.GetTextPayload() != "after" {
		t.Errorf("the metric's samples = %v, want the entry written after it alone", got)
	}

	matched, err := srv.MatchEntries("proj-1", `logName = "projects/proj-1/logs/checkout"`, nil, wall.Add(-time.Second), wall)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].Entry.GetTextPayload() != "after" {
		t.Errorf("the entries received in the last second = %v, want the one written then", matched)
	}
}
