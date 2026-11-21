package main

import "testing"

const envoyStatsSample = `# TYPE envoy_cluster_upstream_rq counter
envoy_cluster_upstream_rq{envoy_response_code="200",envoy_cluster_name="cloudburrow-run/hello-00001"} 7
envoy_cluster_upstream_rq{envoy_response_code="503",envoy_cluster_name="cloudburrow-run/hello-00001"} 1
envoy_cluster_upstream_rq{envoy_response_code="200",envoy_cluster_name="knative-serving/activator-service"} 99
envoy_cluster_upstream_rq_total{envoy_cluster_name="cloudburrow-run/hello-00001"} 8
# TYPE envoy_cluster_upstream_rq_time histogram
envoy_cluster_upstream_rq_time_bucket{envoy_cluster_name="cloudburrow-run/hello-00001",le="0.5"} 0
envoy_cluster_upstream_rq_time_bucket{envoy_cluster_name="cloudburrow-run/hello-00001",le="10"} 5
envoy_cluster_upstream_rq_time_bucket{envoy_cluster_name="cloudburrow-run/hello-00001",le="1000"} 8
envoy_cluster_upstream_rq_time_bucket{envoy_cluster_name="cloudburrow-run/hello-00001",le="+Inf"} 8
envoy_cluster_upstream_rq_time_sum{envoy_cluster_name="cloudburrow-run/hello-00001"} 412.5
envoy_cluster_upstream_rq_time_count{envoy_cluster_name="cloudburrow-run/hello-00001"} 8
envoy_cluster_upstream_rq{envoy_response_code="404",envoy_cluster_name="hello-00002.cloudburrow-run.svc.cluster.local:80"} 2
`

func TestParseEnvoyStatsCountsEachRevision(t *testing.T) {
	by := map[string]*envoyRevision{}
	parseEnvoyStats(envoyStatsSample, "cloudburrow-run", by)
	r := by["hello-00001"]
	if r == nil || r.codes[200] != 7 || r.codes[503] != 1 {
		t.Fatalf("hello-00001: %+v", r)
	}
	if len(r.bounds) != 3 || r.bounds[1] != 10 || r.buckets[1] != 5 || r.count != 8 || r.sum != 412.5 {
		t.Fatalf("latency: %+v", r)
	}
	if by["activator-service"] != nil {
		t.Fatal("another namespace's cluster was counted")
	}
	if r2 := by["hello-00002"]; r2 == nil || r2.codes[404] != 2 {
		t.Fatalf("a host-named cluster: %+v", by)
	}
}

func TestPromLabelsAndClusterNames(t *testing.T) {
	l := promLabels(`a="1",b="x\"y",le="+Inf"`)
	if l["a"] != "1" || l["b"] != `x"y` || l["le"] != "+Inf" {
		t.Fatalf("labels %v", l)
	}
	for in, want := range map[string]string{"ns/rev-00001": "rev-00001", "ns/rev-00001-private": "rev-00001",
		"rev-00001.ns.svc.cluster.local:80": "rev-00001", "other/rev": ""} {
		if got := envoyRevisionName(in, "ns"); got != want {
			t.Errorf("envoyRevisionName(%q) = %q, want %q", in, got, want)
		}
	}
}
