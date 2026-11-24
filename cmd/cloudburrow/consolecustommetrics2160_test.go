package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleCustomMetricsBalancingMode (#2160): Create load balancer's
// backend takes the Custom metrics balancing mode, with the metric's name,
// max utilization and dry run, and refuses a max utilization above 1.0.
func TestConsoleCustomMetricsBalancingMode(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "lb-custom-metrics"
	lb := computeLBProvider{screen}
	if _, err := screen.call("POST", project, "zones/us-central1-a/instanceGroups", nil, map[string]any{"name": "web",
		"namedPorts": []any{map[string]any{"name": "http", "port": float64(80)}}}); err != nil {
		t.Fatal(err)
	}
	fields := lb.CreateFormFor(ctx, project)
	if f := vpcField(fields, "balancingMode"); f == nil || !strings.Contains(strings.Join(f.Options, ","), "CUSTOM_METRICS") ||
		f.OptionLabels["CUSTOM_METRICS"] != "Custom metrics" {
		t.Fatalf("balancing mode field = %+v", f)
	}
	for _, n := range []string{"customMetricName", "customMetricMaxUtilization", "customMetricDryRun"} {
		if f := vpcField(fields, n); f == nil || f.VisibleWhen["balancingMode"] != "CUSTOM_METRICS" {
			t.Fatalf("%s field = %+v", n, f)
		}
	}
	base := map[string]string{"lbType": "application", "facing": "external", "deployment": "global", "name": "orders",
		"protocol": "HTTP", "port": "80", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web", "portName": "http",
		"balancingMode": "CUSTOM_METRICS", "customMetricName": "orders", "customMetricMaxUtilization": "1.5"}
	if _, err := lb.Create(ctx, project, base); err == nil || !strings.Contains(err.Error(), "Max utilization") {
		t.Fatalf("max utilization 1.5: %v", err)
	}
	base["customMetricMaxUtilization"], base["customMetricDryRun"] = "0.6", "true"
	if _, err := lb.Create(ctx, project, base); err != nil {
		t.Fatal(err)
	}
	bs, err := screen.call("GET", project, "global/backendServices/orders-backend-service", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	be, _ := bs["backends"].([]any)[0].(map[string]any)
	m, _ := be["customMetrics"].([]any)[0].(map[string]any)
	if be["balancingMode"] != "CUSTOM_METRICS" || m["name"] != "orders" || m["maxUtilization"] != 0.6 || m["dryRun"] != true {
		t.Fatalf("backend = %v", be)
	}
}
