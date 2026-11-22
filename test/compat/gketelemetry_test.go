//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// gkeLogPod runs the event probe as a pod that writes out to standard
// output and errOut to standard error as it starts.
func gkeLogPod(t *testing.T, kubeconfig, pod, out, errOut string) {
	t.Helper()
	gkeKubectl(t, kubeconfig, fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: default, labels: {app: telemetry-2100}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: probe
    image: %s
    imagePullPolicy: Never
    env: [{name: PROBE_STDOUT, value: %q}, {name: PROBE_STDERR, value: %q}]
`, pod, eventProbeImage, out, errOut), "apply", "-f", "-")
	gkeKubectl(t, kubeconfig, "", "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=180s")
}

// gkeLogEntries are the k8s_container entries of cluster whose text is text.
func gkeLogEntries(ctx context.Context, t *testing.T, ac *logadmin.Client, cluster, text string) []*logging.Entry {
	t.Helper()
	it := ac.Entries(ctx, logadmin.Filter(fmt.Sprintf(`resource.type="k8s_container" AND resource.labels.cluster_name=%q AND textPayload=%q`, cluster, text)))
	var out []*logging.Entry
	for {
		e, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("logadmin Entries: %v", err)
		}
		out = append(out, e)
	}
}

// gkeCPUSeries are the kubernetes.io/container/cpu/core_usage_time series
// of a pod of cluster, with a point after since.
func gkeCPUSeries(ctx context.Context, t *testing.T, h *Harness, cluster, pod string, since time.Time) []*monitoringpb.TimeSeries {
	t.Helper()
	mc := monitoringClient(t, h)
	return listAll(t, mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
		Name: "projects/" + h.Project(),
		Filter: fmt.Sprintf(`metric.type="kubernetes.io/container/cpu/core_usage_time" AND resource.labels.cluster_name=%q AND resource.labels.pod_name=%q`,
			cluster, pod),
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(since), EndTime: timestamppb.New(time.Now())},
		View:     monitoringpb.ListTimeSeriesRequest_FULL,
	}))
}

// TestGKELoggingAndMonitoring (#2100): a cluster's container logs reach
// Cloud Logging and its system metrics Cloud Monitoring, as GKE ships
// them, through the official clients:
//   - a pod's standard output is in projects/P/logs/stdout and its
//     standard error in .../stderr, on k8s_container with the cluster's,
//     namespace's, pod's and container's labels, and the pod's labels as
//     k8s-pod/<key>;
//   - its CPU usage is a kubernetes.io/container/cpu/core_usage_time
//     series on the same resource;
//   - SetLoggingService("none") stops the shipping: a line written then
//     never arrives; desired_logging_config with SYSTEM_COMPONENTS and
//     WORKLOADS starts it again;
//   - SetMonitoringService("none") stops the metrics; turning it back on
//     resumes them;
//   - the legacy services and WORKLOADS without SYSTEM_COMPONENTS are
//     refused.
//
// covers: google.container.v1.ClusterManager/SetLoggingService, google.container.v1.ClusterManager/SetMonitoringService
func TestGKELoggingAndMonitoring(t *testing.T) {
	h := New(t)
	logAddr := h.Endpoint(EnvLogging)
	monitoringClient(t, h)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set: the probe is loaded into the cluster by kind")
	}
	ctx := context.Background()
	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := h.Project()
	const zone, cluster = "us-central1-b", "telemetry-2100"
	name := "projects/" + project + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + zone, Cluster: &containerpb.Cluster{
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
	if c.GetLoggingService() != "logging.googleapis.com/kubernetes" || c.GetMonitoringService() != "monitoring.googleapis.com/kubernetes" ||
		len(c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()) != 2 ||
		len(c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents()) != 10 || !c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled() {
		t.Errorf("a new cluster's logging %q %v, monitoring %q %v", c.GetLoggingService(), c.GetLoggingConfig(), c.GetMonitoringService(), c.GetMonitoringConfig())
	}
	kc := gkeKubeconfig(t, c)
	loadEventProbe(ctx, t)
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", gke.KindName(instance, name)).CombinedOutput(); err != nil {
		t.Fatalf("load the probe into the GKE cluster: %v\n%s", err, b)
	}

	// Logs.
	stamp := fmt.Sprint(time.Now().UnixNano())
	out1, err1 := "stdout-"+stamp+"-1", "stderr-"+stamp+"-1"
	started := time.Now().Add(-time.Minute)
	gkeLogPod(t, kc, "lg1", out1, err1)
	var got []*logging.Entry
	if !gkeEventually(90*time.Second, func() bool { got = gkeLogEntries(ctx, t, ac, cluster, out1); return len(got) > 0 }) {
		t.Fatalf("the pod's standard output is not in Cloud Logging")
	}
	e := got[0]
	if e.LogName != "projects/"+project+"/logs/stdout" {
		t.Errorf("logName = %q", e.LogName)
	}
	want := map[string]string{"project_id": project, "location": zone, "cluster_name": cluster, "namespace_name": "default",
		"pod_name": "lg1", "container_name": "probe"}
	if e.Resource.GetType() != "k8s_container" {
		t.Errorf("resource = %v", e.Resource)
	}
	for k, v := range want {
		if e.Resource.GetLabels()[k] != v {
			t.Errorf("resource label %s = %q, want %q", k, e.Resource.GetLabels()[k], v)
		}
	}
	if e.Labels["k8s-pod/app"] != "telemetry-2100" || e.Labels["compute.googleapis.com/resource_name"] == "" {
		t.Errorf("entry labels = %v", e.Labels)
	}
	// Shipped by the cluster's own agent, as GKE's: the kube-system
	// DaemonSet fluentbit-gke, one ready pod on each node.
	if ds := gkeKubectl(t, kc, "", "get", "daemonset", "-n", "kube-system", "fluentbit-gke", "-o",
		"jsonpath={.status.desiredNumberScheduled} {.status.numberReady}"); len(strings.Fields(ds)) != 2 ||
		strings.Fields(ds)[0] != strings.Fields(ds)[1] || strings.Fields(ds)[0] == "0" {
		t.Errorf("kube-system/fluentbit-gke desired and ready = %q", ds)
	}
	if !gkeEventually(30*time.Second, func() bool { got = gkeLogEntries(ctx, t, ac, cluster, err1); return len(got) > 0 }) {
		t.Fatalf("the pod's standard error is not in Cloud Logging")
	}
	if got[0].LogName != "projects/"+project+"/logs/stderr" || got[0].Severity != logging.Error {
		t.Errorf("stderr entry: log %q severity %v", got[0].LogName, got[0].Severity)
	}

	// Metrics: the sampler reads the kubelet once a minute.
	if !gkeEventually(150*time.Second, func() bool { return len(gkeCPUSeries(ctx, t, h, cluster, "lg1", started)) > 0 }) {
		t.Fatal("no kubernetes.io/container/cpu/core_usage_time series for the pod")
	}
	ts := gkeCPUSeries(ctx, t, h, cluster, "lg1", started)[0]
	if ts.GetResource().GetType() != "k8s_container" || ts.GetResource().GetLabels()["container_name"] != "probe" ||
		ts.GetMetricKind().String() != "CUMULATIVE" || ts.GetValueType().String() != "DOUBLE" {
		t.Errorf("series = %v", ts)
	}

	// Validation.
	_, err = g.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: "logging.googleapis.com"})
	wantPrivateCACode(t, "the legacy logging service", err, codes.InvalidArgument)
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{
			EnableComponents: []containerpb.LoggingComponentConfig_Component{containerpb.LoggingComponentConfig_WORKLOADS}}}}})
	wantPrivateCACode(t, "workload logs without system logs", err, codes.InvalidArgument)

	// Logging off: nothing written meanwhile is shipped, then or later.
	op, err = r.SetLoggingService(ctx, &containerpb.SetLoggingServiceRequest{Name: name, LoggingService: "none"})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	if c, _ := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); c.GetLoggingService() != "none" ||
		len(c.GetLoggingConfig().GetComponentConfig().GetEnableComponents()) != 0 {
		t.Errorf("after none: %q %v", c.GetLoggingService(), c.GetLoggingConfig())
	}
	time.Sleep(5 * time.Second)
	if !gkeEventually(60*time.Second, func() bool {
		return gkeKubectl(t, kc, "", "get", "daemonset", "-n", "kube-system", "fluentbit-gke", "--ignore-not-found", "-o", "name") == ""
	}) {
		t.Error("the fluentbit-gke DaemonSet is still there with logging none")
	}
	out2 := "stdout-" + stamp + "-2"
	gkeLogPod(t, kc, "lg2", out2, "")
	time.Sleep(20 * time.Second)
	if got := gkeLogEntries(ctx, t, ac, cluster, out2); len(got) != 0 {
		t.Fatalf("a line written with logging none was shipped: %v", got[0])
	}
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredLoggingConfig: &containerpb.LoggingConfig{ComponentConfig: &containerpb.LoggingComponentConfig{
			EnableComponents: []containerpb.LoggingComponentConfig_Component{containerpb.LoggingComponentConfig_SYSTEM_COMPONENTS,
				containerpb.LoggingComponentConfig_WORKLOADS}}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	out3 := "stdout-" + stamp + "-3"
	gkeLogPod(t, kc, "lg3", out3, "")
	if !gkeEventually(90*time.Second, func() bool { return len(gkeLogEntries(ctx, t, ac, cluster, out3)) > 0 }) {
		t.Fatal("logs are not shipped again after SYSTEM_COMPONENTS and WORKLOADS")
	}
	if got := gkeLogEntries(ctx, t, ac, cluster, out2); len(got) != 0 {
		t.Errorf("the line written with logging none was shipped once logging was back: %v", got[0])
	}

	// Monitoring off, then on.
	op, err = g.SetMonitoringService(ctx, &containerpb.SetMonitoringServiceRequest{Name: name, MonitoringService: "none"})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	off := time.Now().Add(10 * time.Second)
	time.Sleep(80 * time.Second)
	for _, s := range gkeCPUSeries(ctx, t, h, cluster, "lg1", off) {
		t.Errorf("a point was written with monitoring none: %v", s.GetPoints())
	}
	op, err = r.SetMonitoringService(ctx, &containerpb.SetMonitoringServiceRequest{Name: name, MonitoringService: "monitoring.googleapis.com/kubernetes"})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	on := time.Now()
	if !gkeEventually(150*time.Second, func() bool { return len(gkeCPUSeries(ctx, t, h, cluster, "lg1", on)) > 0 }) {
		t.Fatal("metrics are not shipped again after monitoring.googleapis.com/kubernetes")
	}
}

// TestGcloudContainerClusterLoggingAndMonitoring (#2100): gcloud's
// --logging and --monitoring, on create and update, and `gcloud logging
// read` finds a pod's line on k8s_container.
//
// covers: google.container.v1.ClusterManager/SetLoggingService, google.container.v1.ClusterManager/SetMonitoringService
func TestGcloudContainerClusterLoggingAndMonitoring(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvMonitoring)
	g, _ := gkeClients(t, h)
	shardEndpoint(h, "CLOUDBURROW_TEST_GKE_SERVICEUSAGE", EnvServiceUsage)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set: the probe is loaded into the cluster by kind")
	}
	gdir := t.TempDir()
	cfg := gcloudSetup(t, cli, gdir)
	kubeconfig := t.TempDir() + "/kubeconfig"
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfg,
			"CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true", "KUBECONFIG="+kubeconfig)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	const zone, cluster = "us-central1-a", "gc-telemetry-2100"
	full := "projects/" + h.Project() + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, h.Project(), zone, op)
		}
	})
	describe := func(format string) string {
		t.Helper()
		return strings.TrimSpace(gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value("+format+")"))
	}
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate",
		"--logging=SYSTEM,WORKLOAD", "--monitoring=SYSTEM")
	if got := describe("loggingConfig.componentConfig.enableComponents"); !strings.Contains(got, "SYSTEM_COMPONENTS") || !strings.Contains(got, "WORKLOADS") {
		t.Errorf("logging components after create = %q", got)
	}
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)

	ctx := context.Background()
	loadEventProbe(ctx, t)
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", gke.KindName(instance, full)).CombinedOutput(); err != nil {
		t.Fatalf("load the probe into the GKE cluster: %v\n%s", err, b)
	}
	line := fmt.Sprintf("gcloud-telemetry-%d", time.Now().UnixNano())
	gkeLogPod(t, kubeconfig, "gl1", line, "")
	filter := fmt.Sprintf(`resource.type="k8s_container" AND resource.labels.cluster_name=%q AND textPayload=%q`, cluster, line)
	if !gkeEventually(90*time.Second, func() bool {
		return strings.Contains(gc("logging", "read", filter, "--format=value(resource.labels.pod_name)"), "gl1")
	}) {
		t.Fatal("gcloud logging read does not find the pod's line")
	}

	gc("container", "clusters", "update", cluster, "--zone", zone, "--logging=NONE")
	if got := describe("loggingService"); got != "none" {
		t.Errorf("loggingService after --logging=NONE = %q", got)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--logging=SYSTEM")
	if got := describe("loggingConfig.componentConfig.enableComponents"); !strings.Contains(got, "SYSTEM_COMPONENTS") || strings.Contains(got, "WORKLOADS") {
		t.Errorf("logging components after --logging=SYSTEM = %q", got)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--monitoring=NONE")
	if got := describe("monitoringService"); got != "none" {
		t.Errorf("monitoringService after --monitoring=NONE = %q", got)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--monitoring=SYSTEM")
	if got := describe("monitoringService"); got != "monitoring.googleapis.com/kubernetes" {
		t.Errorf("monitoringService after --monitoring=SYSTEM = %q", got)
	}
}
