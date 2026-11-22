//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestGKEMetricPackagesAndManagedPrometheus (#2100): GKE's metric
// packages and Managed Service for Prometheus, through the official
// container and monitoring clients and the Prometheus HTTP API:
//   - a new cluster has system metrics, kube state metrics and
//     cAdvisor/Kubelet on, and managed collection on, as GKE's defaults;
//   - its e2-medium node's allocatable is GKE's: 940m of CPU (1060m
//     reserved of a shared-core machine) and its memory less GKE's
//     reservation;
//   - kube state metrics' kube_deployment_spec_replicas reads a
//     Deployment's replicas, and cAdvisor's
//     container_cpu_usage_seconds_total its container, on prometheus_target;
//   - with APISERVER, SCHEDULER and CONTROLLER_MANAGER on, the API
//     server's apiserver_request_total is a prometheus.googleapis.com
//     counter, answered by PromQL through Monitoring's Prometheus API;
//   - a PodMonitoring selecting CoreDNS' pods (port metrics) is accepted
//     (ConfigurationCreateSuccess) and its coredns_* metrics arrive, with
//     the job, instance and namespace labels; a ClusterPodMonitoring too;
//   - kube state metrics without managed collection are refused, and
//     turning managed collection off with them on is refused.
//
// covers: google.container.v1.ClusterManager/UpdateCluster
func TestGKEMetricPackagesAndManagedPrometheus(t *testing.T) {
	h := New(t)
	monAddr := h.Endpoint(EnvMonitoring)
	mc := monitoringClient(t, h)
	g, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-b", "prom-2100"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: cluster, MaintenancePolicy: gkeNoAutoUpgrade(),
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	comps := map[string]bool{}
	for _, m := range c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents() {
		comps[m.String()] = true
	}
	for _, want := range []string{"SYSTEM_COMPONENTS", "STORAGE", "POD", "DEPLOYMENT", "STATEFULSET", "DAEMONSET", "HPA", "CADVISOR", "KUBELET"} {
		if !comps[want] {
			t.Errorf("a new cluster's monitoring lacks %s: %v", want, c.GetMonitoringConfig())
		}
	}
	if !c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled() {
		t.Errorf("managed collection is off on a new cluster: %v", c.GetMonitoringConfig())
	}
	kc := gkeKubeconfig(t, c)

	// The node's allocatable is a GKE e2-medium's.
	alloc := gkeKubectl(t, kc, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool=default-pool", "-o",
		"jsonpath={.items[0].status.allocatable.cpu} {.items[0].status.allocatable.memory}")
	f := strings.Fields(alloc)
	if len(f) != 2 || f[0] != "940m" {
		t.Errorf("an e2-medium node's allocatable = %q, want 940m of CPU", alloc)
	} else if ki, err := strconv.ParseInt(strings.TrimSuffix(f[1], "Ki"), 10, 64); err != nil || ki/1024 != 4096-1024-100 {
		t.Errorf("an e2-medium node's allocatable memory = %s, want %d MiB", f[1], 4096-1024-100)
	}
	limits := gkeKubectl(t, kc, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool=default-pool", "-o", "jsonpath={.items[0].metadata.name}")
	if out, err := exec.Command("docker", "inspect", "-f", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", strings.TrimSpace(limits)).CombinedOutput(); err != nil ||
		strings.TrimSpace(string(out)) != "2000000000 4294967296" {
		t.Errorf("the node container's limits = %q (%v), want 2 CPUs and 4 GiB", out, err)
	}

	// Kube state and cAdvisor: a Deployment in its namespace, of the
	// node's own pause image (nothing is pulled).
	node := strings.TrimSpace(limits)
	out, err := exec.Command("docker", "exec", node, "sh", "-c", "ctr -n k8s.io images ls -q | grep '/pause:' | head -1").Output()
	pause := strings.TrimSpace(string(out))
	if err != nil || pause == "" {
		t.Fatalf("the node's pause image: %q %v", out, err)
	}
	gkeKubectl(t, kc, `apiVersion: v1
kind: Namespace
metadata: {name: prom}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: prom}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - name: pause
        image: `+pause+`
        imagePullPolicy: Never
`, "apply", "-f", "-")
	series := func(metricType, filter string) []*monitoringpb.TimeSeries {
		return listAll(t, mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
			Name:     "projects/" + project,
			Filter:   fmt.Sprintf(`metric.type=%q AND resource.type="prometheus_target" AND resource.labels.cluster=%q`, metricType, cluster) + filter,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-10 * time.Minute)), EndTime: timestamppb.New(time.Now())},
		}))
	}
	waitSeries := func(what, metricType, filter string, ok func([]*monitoringpb.TimeSeries) bool) []*monitoringpb.TimeSeries {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for {
			s := series(metricType, filter)
			if ok(s) {
				return s
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: %d series of %s", what, len(s), metricType)
				return s
			}
			time.Sleep(5 * time.Second)
		}
	}
	waitSeries("kube state metrics", "prometheus.googleapis.com/kube_deployment_spec_replicas/gauge",
		` AND resource.labels.namespace="prom" AND metric.labels.deployment="web"`, func(s []*monitoringpb.TimeSeries) bool {
			return len(s) == 1 && s[0].GetPoints()[0].GetValue().GetDoubleValue() == 2 && s[0].GetResource().GetLabels()["job"] == "kube-state-metrics"
		})
	waitSeries("cAdvisor", "prometheus.googleapis.com/container_cpu_usage_seconds_total/counter",
		` AND resource.labels.namespace="prom"`, func(s []*monitoringpb.TimeSeries) bool {
			return len(s) > 0 && s[0].GetMetricKind().String() == "CUMULATIVE"
		})

	// The control plane's packages.
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS,
				containerpb.MonitoringComponentConfig_APISERVER, containerpb.MonitoringComponentConfig_SCHEDULER,
				containerpb.MonitoringComponentConfig_CONTROLLER_MANAGER, containerpb.MonitoringComponentConfig_POD}}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	waitSeries("the API server", "prometheus.googleapis.com/apiserver_request_total/counter", ` AND metric.labels.verb="LIST"`,
		func(s []*monitoringpb.TimeSeries) bool { return len(s) > 0 })
	waitSeries("the scheduler", "prometheus.googleapis.com/scheduler_schedule_attempts_total/counter", "",
		func(s []*monitoringpb.TimeSeries) bool { return len(s) > 0 })
	base := "http://" + monAddr + "/v1/projects/" + project + "/location/global/prometheus/api/v1/"
	promQuery := func(q string) []any {
		t.Helper()
		resp, err := http.Get(base + "query?" + url.Values{"query": {q}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m struct {
			Status string `json:"status"`
			Data   struct {
				Result []any `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &m); err != nil || m.Status != "success" {
			t.Errorf("PromQL %s: %s", q, b)
		}
		return m.Data.Result
	}
	if r := promQuery(fmt.Sprintf(`sum by (verb) (apiserver_request_total{cluster=%q})`, cluster)); len(r) == 0 {
		t.Error("PromQL over apiserver_request_total answered nothing")
	}
	if r := promQuery(fmt.Sprintf(`histogram_quantile(0.9, sum by (le) (rate(apiserver_request_duration_seconds_bucket{cluster=%q}[10m])))`, cluster)); len(r) != 1 {
		t.Errorf("PromQL over the API server's latency histogram answered %v", r)
	}

	// Managed collection: a PodMonitoring of CoreDNS, a ClusterPodMonitoring too.
	gkeKubectl(t, kc, `apiVersion: monitoring.googleapis.com/v1
kind: PodMonitoring
metadata: {name: coredns, namespace: kube-system}
spec:
  selector: {matchLabels: {k8s-app: kube-dns}}
  endpoints:
  - port: metrics
    interval: 10s
    metricRelabeling:
    - sourceLabels: [__name__]
      regex: coredns_(build_info|dns_requests_total)
      action: keep
---
apiVersion: monitoring.googleapis.com/v1
kind: ClusterPodMonitoring
metadata: {name: coredns-all}
spec:
  selector: {matchLabels: {k8s-app: kube-dns}}
  endpoints:
  - port: 9153
    interval: 15s
`, "apply", "-f", "-")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st := gkeKubectl(t, kc, "", "-n", "kube-system", "get", "podmonitoring", "coredns", "-o",
			`jsonpath={.status.conditions[?(@.type=="ConfigurationCreateSuccess")].status}`)
		if strings.TrimSpace(st) == "True" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the PodMonitoring's ConfigurationCreateSuccess = %q", st)
			break
		}
		time.Sleep(3 * time.Second)
	}
	s := waitSeries("a PodMonitoring", "prometheus.googleapis.com/coredns_build_info/gauge", ` AND resource.labels.job="coredns"`,
		func(s []*monitoringpb.TimeSeries) bool { return len(s) > 0 })
	if len(s) > 0 {
		r := s[0].GetResource().GetLabels()
		if r["namespace"] != "kube-system" || !strings.HasSuffix(r["instance"], ":metrics") || r["location"] != zone || s[0].GetMetric().GetLabels()["pod"] == "" {
			t.Errorf("a PodMonitoring series' labels: %v %v", r, s[0].GetMetric().GetLabels())
		}
	}
	if dropped := series("prometheus.googleapis.com/coredns_cache_entries/gauge", ` AND resource.labels.job="coredns"`); len(dropped) > 0 {
		t.Errorf("metricRelabeling's keep kept coredns_cache_entries: %d series", len(dropped))
	}
	waitSeries("a ClusterPodMonitoring", "prometheus.googleapis.com/coredns_build_info/gauge", ` AND resource.labels.job="coredns-all"`,
		func(s []*monitoringpb.TimeSeries) bool { return len(s) > 0 })
	if r := promQuery(fmt.Sprintf(`count(coredns_build_info{cluster=%q, job="coredns"})`, cluster)); len(r) != 1 {
		t.Errorf("PromQL over the PodMonitoring's metric answered %v", r)
	}

	// The packages managed collection serves need it.
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{Enabled: false}}}})
	wantPrivateCACode(t, "managed collection off under kube state metrics", err, codes.InvalidArgument)
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS}},
			ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{Enabled: false}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS,
				containerpb.MonitoringComponentConfig_DEPLOYMENT}}}}})
	wantPrivateCACode(t, "kube state metrics without managed collection", err, codes.InvalidArgument)
}
