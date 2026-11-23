package compute

import (
	"net/http"
	"testing"
)

// TestCustomMetricSamplesIncludeDryRun: a dry-run metric's reported values
// are sampled for Cloud Monitoring beside the metric used for balancing.
func TestCustomMetricSamplesIncludeDryRun(t *testing.T) {
	s := customMetricsFixture(t)
	h, p := reporting(t, "one", "TEXT named_metrics.orders=0.4, named_metrics.queue=0.7")
	g := hybridNEG(t, s, "us-central1-a", "x", [2]any{h, p})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "dry", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"},
		"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{
			obj{"name": "orders", "maxUtilization": 0.8}, obj{"name": "queue", "dryRun": true}}}}})
	u := lbFront(t, s, "dry")
	c := &http.Client{}
	var got []CustomMetricSample
	waitFor(t, "samples of both metrics", func() bool {
		if resp, err := c.Get(u + "/"); err == nil {
			resp.Body.Close()
		}
		got = s.CustomMetricSamples()
		return len(got) == 2
	})
	by := map[string]CustomMetricSample{}
	for _, m := range got {
		by[m.Metric] = m
	}
	q, o := by["queue"], by["orders"]
	if !q.DryRun || q.Value != 0.7 || q.GroupType != "NETWORK_ENDPOINT_GROUP" || q.Scope != "us-central1-a" || q.ScopeType != "ZONE" ||
		q.BackendService != "dry" || q.Region != "global" || q.GroupName != "x" || q.Project == "" {
		t.Fatalf("queue: %+v", q)
	}
	if o.DryRun || o.Value != 0.4 {
		t.Fatalf("orders: %+v", o)
	}
}
