package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

type consoleRunSource struct{ revs []monitoring.RunRevision }

func (s *consoleRunSource) RunRevisions() ([]monitoring.RunRevision, error) { return s.revs, nil }

func TestRunMetricsSectionChartsGooglesThreeMetrics(t *testing.T) {
	api, err := monitoring.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(-2 * time.Minute)
	rev := func(ok, timed uint64, active int64) monitoring.RunRevision {
		return monitoring.RunRevision{Project: "my-project", Location: "us-central1", Service: "hello", Revision: "hello-00001-x",
			Configuration: "hello", Requests: map[int]uint64{200: ok}, LatencyBounds: []float64{10, 100},
			LatencyBuckets: []uint64{timed, timed}, LatencyCount: timed, LatencySum: float64(timed) * 5, Active: active}
	}
	src := &consoleRunSource{revs: []monitoring.RunRevision{rev(0, 0, 0)}}
	b := monitoring.NewRunBuiltin(src, func() time.Time { return clock })
	api.AddBuiltin(b)
	clock = clock.Add(time.Minute)
	src.revs = []monitoring.RunRevision{rev(6, 6, 1)}
	b.Sample()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	api.Register(g)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	setRunMetricsEndpoint(lis.Addr().String)
	defer setRunMetricsEndpoint(nil)

	sec := runMetricsSection(context.Background(), "my-project", "us-central1", "hello")
	if sec.Kind != console.KindChart {
		t.Fatalf("section %+v", sec)
	}
	labels := map[string]bool{}
	for _, s := range sec.Series {
		labels[s.Label] = len(s.Points) > 0
	}
	for _, want := range []string{"Request count (2xx)", "Request latencies: 99%", "Request latencies: 95%", "Request latencies: 50%",
		"Container instance count (active)"} {
		if !labels[want] {
			t.Errorf("no points for %q in %v", want, labels)
		}
	}

	// Another service has its charts, with no readings.
	other := runMetricsSection(context.Background(), "my-project", "us-central1", "other")
	for _, s := range other.Series {
		if len(s.Points) != 0 {
			t.Fatalf("other service has %v", s)
		}
	}

	// A function's tab is its service's.
	fn := &functionspb.Function{Name: "projects/my-project/locations/us-central1/functions/hello"}
	if f := functionMetricsSection(context.Background(), fn); f.Kind != console.KindChart || len(f.Series) != len(sec.Series) {
		t.Fatalf("function metrics %+v", f)
	}
}

func TestRunMetricsSectionWithoutMonitoring(t *testing.T) {
	setRunMetricsEndpoint(func() string { return "" })
	defer setRunMetricsEndpoint(nil)
	sec := runMetricsSection(context.Background(), "my-project", "us-central1", "hello")
	if sec.Kind != console.KindText || !strings.Contains(sec.Text, "not served") {
		t.Fatalf("section %+v", sec)
	}
}
