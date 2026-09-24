package console

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

func TestQuantileInterpolatesWithinTheBucket(t *testing.T) {
	// 100 calls, all between 10ms and 25ms: every quantile lies in that bucket.
	cum := make([]uint64, len(metrics.Buckets))
	for i, b := range metrics.Buckets {
		if b >= 0.025 {
			cum[i] = 100
		}
	}
	if got := quantile(0.5, cum, 100); math.Abs(got-0.0175) > 1e-9 {
		t.Errorf("p50 = %v, want the bucket's midpoint 0.0175", got)
	}
	// Past the last finite bound, the bound is all that is known.
	if got := quantile(0.95, make([]uint64, len(metrics.Buckets)), 10); got != metrics.Buckets[len(metrics.Buckets)-1] {
		t.Errorf("an unbounded quantile = %v", got)
	}
}

// TestRequestChartsComputeRatesErrorsAndLatency drives the series with a fixed
// clock: 10 calls in a 5s interval, 2 of them failed, is 2/s at 20% errors.
func TestRequestChartsComputeRatesErrorsAndLatency(t *testing.T) {
	reg := metrics.New("storage")
	s := New("127.0.0.1:0", nil)
	s.SetRequestMetrics(reg)
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s.SampleRequests(t0)
	for i := 0; i < 8; i++ {
		reg.Observe("tasks", "CreateTask", "OK", 3*time.Millisecond)
	}
	reg.Observe("tasks", "GetQueue", "NOT_FOUND", 1*time.Millisecond)
	reg.Observe("secretmanager", "JSON GET", "404", 2*time.Millisecond)
	reg.Observe("tasks", "GetTask", "NOT_FOUND", 1*time.Millisecond)
	s.SampleRequests(t0.Add(5 * time.Second))

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/metrics/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Services []struct {
			Service string
			Points  []RequestPoint
		}
		Unmeasured      []string
		UnmeasuredLabel string `json:"unmeasured_label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got := map[string]RequestPoint{}
	for _, svc := range body.Services {
		if len(svc.Points) != 1 {
			t.Fatalf("%s has %d points, want 1", svc.Service, len(svc.Points))
		}
		got[svc.Service] = svc.Points[0]
	}
	tp := got["tasks"]
	if tp.Rate != 2 || tp.ErrorRate == nil || math.Abs(*tp.ErrorRate-0.2) > 1e-9 {
		t.Errorf("tasks = rate %v, errors %v; want 2/s at 20%%", tp.Rate, tp.ErrorRate)
	}
	if tp.P50MS == nil || *tp.P50MS <= 1 || *tp.P50MS > 5 {
		t.Errorf("tasks p50 = %v ms, want within the 1-5 ms bucket", tp.P50MS)
	}
	if sm := got["secretmanager"]; sm.ErrorRate == nil || *sm.ErrorRate != 1 {
		t.Errorf("an HTTP 404 was not counted as a failure: %+v", sm)
	}
	if len(body.Unmeasured) != 1 || body.Unmeasured[0] != "storage" || body.UnmeasuredLabel != NotMeasured {
		t.Errorf("unmeasured = %v %q", body.Unmeasured, body.UnmeasuredLabel)
	}
}
