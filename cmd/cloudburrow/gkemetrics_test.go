package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

const summaryJSON = `{"node":{"nodeName":"n1","startTime":"2026-11-21T10:00:00Z",
 "cpu":{"time":"2026-11-21T12:00:00Z","usageCoreNanoSeconds":3000000000},
 "memory":{"time":"2026-11-21T12:00:00Z","usageBytes":1000,"workingSetBytes":600},
 "network":{"time":"2026-11-21T12:00:00Z","name":"eth0","rxBytes":10,"txBytes":20,"interfaces":[{"name":"eth0","rxBytes":10,"txBytes":20}]},
 "fs":{"time":"2026-11-21T12:00:00Z","availableBytes":50,"capacityBytes":100,"usedBytes":40}},
 "pods":[{"podRef":{"name":"web-1","namespace":"default","uid":"u1"},"startTime":"2026-11-21T11:00:00Z",
  "containers":[{"name":"app","startTime":"2026-11-21T11:00:01Z",
    "cpu":{"time":"2026-11-21T12:00:00Z","usageCoreNanoSeconds":1500000000},
    "memory":{"time":"2026-11-21T12:00:00Z","usageBytes":300,"workingSetBytes":200},
    "rootfs":{"usedBytes":7},"logs":{"usedBytes":3}}],
  "network":{"time":"2026-11-21T12:00:00Z","name":"eth0","rxBytes":5,"txBytes":6,"interfaces":[{"name":"eth0","rxBytes":5,"txBytes":6}]},
  "ephemeral-storage":{"usedBytes":11}}]}`

// TestSummaryPoints (#2100): a kubelet summary's readings are GKE's
// kubernetes.io metrics, on k8s_container, k8s_pod and k8s_node.
func TestSummaryPoints(t *testing.T) {
	var s gkeSummary
	if err := json.Unmarshal([]byte(summaryJSON), &s); err != nil {
		t.Fatal(err)
	}
	tt := gke.TelemetryTarget{Project: "p", Location: "us-central1-a", Cluster: "c1", SystemMetrics: true}
	pts := summaryPoints(tt, s, 90, time.Now())
	got := map[string]monitoring.GKEPoint{}
	for _, p := range pts {
		k := p.Metric
		if mt := p.MetricLabels["memory_type"]; mt != "" {
			k += "/" + mt
		}
		got[k] = p
	}
	check := func(k string, i int64, d float64) {
		t.Helper()
		p, ok := got[k]
		if !ok {
			t.Errorf("no %s", k)
			return
		}
		if p.Int != i || p.Double != d {
			t.Errorf("%s = %d %v, want %d %v", k, p.Int, p.Double, i, d)
		}
	}
	check("kubernetes.io/container/cpu/core_usage_time", 0, 1.5)
	check("kubernetes.io/container/memory/used_bytes/non-evictable", 200, 0)
	check("kubernetes.io/container/memory/used_bytes/evictable", 100, 0)
	check("kubernetes.io/container/ephemeral_storage/used_bytes", 10, 0)
	check("kubernetes.io/pod/network/received_bytes_count", 5, 0)
	check("kubernetes.io/pod/network/sent_bytes_count", 6, 0)
	check("kubernetes.io/pod/ephemeral_storage/used_bytes", 11, 0)
	check("kubernetes.io/node/cpu/core_usage_time", 0, 3)
	check("kubernetes.io/node/memory/used_bytes/non-evictable", 600, 0)
	check("kubernetes.io/node/network/received_bytes_count", 10, 0)
	check("kubernetes.io/node/ephemeral_storage/total_bytes", 100, 0)
	check("kubernetes.io/node/ephemeral_storage/allocatable_bytes", 90, 0)
	c := got["kubernetes.io/container/cpu/core_usage_time"]
	if c.Resource["container_name"] != "app" || c.Resource["pod_name"] != "web-1" || c.Resource["cluster_name"] != "c1" || !c.Start.Equal(time.Date(2026, 11, 21, 11, 0, 1, 0, time.UTC)) {
		t.Errorf("container point = %+v", c)
	}
	if n := got["kubernetes.io/node/cpu/core_usage_time"]; n.Resource["node_name"] != "n1" || len(n.Resource) != 4 {
		t.Errorf("node resource = %v", n.Resource)
	}
	for _, p := range pts {
		if _, ok := monitoring.GKEMetrics[p.Metric]; !ok {
			t.Errorf("%s is not a served metric", p.Metric)
		}
	}

	b := monitoring.NewGKEBuiltin()
	b.Record(pts...)
	ts, err := b.Series("p", "kubernetes.io/container/cpu/core_usage_time", time.Time{}, time.Now().Add(1000*time.Hour))
	if err != nil || len(ts) != 1 || ts[0].Resource.Type != "k8s_container" || ts[0].Points[0].Value.GetDoubleValue() != 1.5 {
		t.Errorf("series = %v, %v", ts, err)
	}
	if ts, _ := b.Series("other", "kubernetes.io/container/cpu/core_usage_time", time.Time{}, time.Now().Add(1000*time.Hour)); len(ts) != 0 {
		t.Errorf("another project's series = %v", ts)
	}
}

func TestParseQuantity(t *testing.T) {
	for q, want := range map[string]int64{"1024": 1024, "1Ki": 1024, "2Mi": 2 << 20, "1G": 1e9, "x": -1} {
		if got := parseQuantity(q); got != want {
			t.Errorf("parseQuantity(%q) = %d, want %d", q, got, want)
		}
	}
}
