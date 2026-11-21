package monitoring

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeRunSource struct{ revs []RunRevision }

func (f *fakeRunSource) RunRevisions() ([]RunRevision, error) { return f.revs, nil }

func runRev(project string, ok, notFound, timed uint64, buckets []uint64, sum float64, active int64) RunRevision {
	return RunRevision{Project: project, Location: "us-central1", Service: "hello", Revision: "hello-00001-abc", Configuration: "hello",
		Requests: map[int]uint64{200: ok, 404: notFound}, LatencyBounds: []float64{10, 100},
		LatencyBuckets: buckets, LatencyCount: timed, LatencySum: sum, Active: active}
}

func TestRunBuiltinServesGooglesRunMetrics(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	src := &fakeRunSource{revs: []RunRevision{runRev("p", 5, 0, 5, []uint64{5, 5}, 20, 1), runRev("other", 1, 0, 1, []uint64{1, 1}, 1, 1)}}
	b := NewRunBuiltin(src, func() time.Time { return now })

	ds := b.Descriptors("p")
	if len(ds) != 3 || ds[0].GetDisplayName() != "Request Count" || ds[1].GetUnit() != "ms" || ds[2].GetMetricKind() != gauge {
		t.Fatalf("descriptors: %v", ds)
	}

	// Two requests answered 200 and one 404 since the start: 1 under 10 ms,
	// 1 under 100 ms, 1 over.
	now = now.Add(time.Minute)
	src.revs = []RunRevision{runRev("p", 7, 1, 8, []uint64{6, 7}, 320, 2), runRev("other", 9, 0, 1, []uint64{1, 1}, 1, 1)}
	b.Sample()

	end := now.Add(time.Second)
	counts, _ := b.Series("p", RunRequestCount, now.Add(-time.Hour), end)
	if len(counts) != 2 {
		t.Fatalf("request_count series = %d, want 200 and 404", len(counts))
	}
	byCode := map[string]int64{}
	for _, s := range counts {
		if s.GetResource().GetType() != "cloud_run_revision" || s.GetResource().GetLabels()["service_name"] != "hello" {
			t.Fatalf("resource %v", s.GetResource())
		}
		byCode[s.GetMetric().GetLabels()["response_code"]] = s.GetPoints()[0].GetValue().GetInt64Value()
		if s.GetMetricKind() != delta || !s.GetPoints()[0].GetInterval().GetStartTime().AsTime().Before(s.GetPoints()[0].GetInterval().GetEndTime().AsTime()) {
			t.Fatalf("not a DELTA point: %v", s)
		}
	}
	if byCode["200"] != 2 || byCode["404"] != 1 {
		t.Fatalf("counts by code %v", byCode)
	}
	if c := counts[0].GetMetric().GetLabels()["response_code_class"]; c != "2xx" && c != "4xx" {
		t.Fatalf("response_code_class %q", c)
	}

	lat, _ := b.Series("p", RunRequestLatencies, now.Add(-time.Hour), end)
	if len(lat) != 1 {
		t.Fatalf("latency series %d", len(lat))
	}
	d := lat[0].GetPoints()[0].GetValue().GetDistributionValue()
	if d.GetCount() != 3 || d.GetMean() != 100 || len(d.GetBucketCounts()) != 3 ||
		d.GetBucketCounts()[0] != 1 || d.GetBucketCounts()[1] != 1 || d.GetBucketCounts()[2] != 1 {
		t.Fatalf("distribution %v", d)
	}

	inst, _ := b.Series("p", RunInstanceCount, now.Add(-time.Hour), end)
	active := int64(-1)
	for _, s := range inst {
		if s.GetMetric().GetLabels()["state"] == "active" {
			active = s.GetPoints()[len(s.GetPoints())-1].GetValue().GetInt64Value()
		}
		if s.GetMetricKind() != gauge {
			t.Fatalf("instance_count kind %v", s.GetMetricKind())
		}
	}
	if active != 2 {
		t.Fatalf("active instances %d", active)
	}

	// Another project's revision is its own.
	other, _ := b.Series("other", RunRequestCount, now.Add(-time.Hour), end)
	if len(other) != 1 || other[0].GetPoints()[0].GetValue().GetInt64Value() != 8 {
		t.Fatalf("other project %v", other)
	}

	// A counter that went down restarted from zero: its value is the change.
	now = now.Add(time.Minute)
	src.revs = []RunRevision{runRev("p", 3, 1, 3, []uint64{3, 3}, 6, 0)}
	b.Sample()
	counts, _ = b.Series("p", RunRequestCount, now.Add(-time.Second), now.Add(time.Second))
	if len(counts) != 1 || counts[0].GetPoints()[0].GetValue().GetInt64Value() != 3 {
		t.Fatalf("after a restart %v", counts)
	}
}

func TestRunBuiltinThroughListTimeSeries(t *testing.T) {
	s, clock := newTestServer(t, "")
	src := &fakeRunSource{revs: []RunRevision{runRev("my-project", 0, 0, 0, nil, 0, 0)}}
	b := NewRunBuiltin(src, func() time.Time { return *clock })
	s.AddBuiltin(b)
	*clock = clock.Add(time.Minute)
	now := *clock
	src.revs = []RunRevision{runRev("my-project", 4, 0, 4, []uint64{4, 4}, 8, 1)}
	b.Sample()
	resp, err := s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/my-project",
		Filter:   `metric.type = "run.googleapis.com/request_count" AND resource.labels.service_name = "hello"`,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now.Add(time.Second))}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTimeSeries()) != 1 || resp.GetTimeSeries()[0].GetPoints()[0].GetValue().GetInt64Value() != 4 {
		t.Fatalf("ListTimeSeries %v", resp.GetTimeSeries())
	}
}
