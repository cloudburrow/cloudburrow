//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// gkeUpgradeVersions are the two newest served versions, the older (N)
// and the newer (N+1); the test skips when one version is served.
func gkeUpgradeVersions(t *testing.T, g *container.ClusterManagerClient, parent string) (string, string, *containerpb.ServerConfig) {
	t.Helper()
	cfg, err := g.GetServerConfig(context.Background(), &containerpb.GetServerConfigRequest{Name: parent})
	if err != nil {
		t.Fatalf("GetServerConfig: %v", err)
	}
	v := cfg.GetValidMasterVersions()
	if len(v) < 2 {
		t.Skipf("one Kubernetes Engine version is served (%v): nothing to upgrade to", v)
	}
	return v[1], v[0], cfg
}

// gkeNoAutoUpgrade is a maintenance policy with a "no upgrades" exclusion
// over the test, so no automatic upgrade races the manual ones, which
// ignore exclusions. It lasts 29 days (a "no upgrades" exclusion may last
// 30): TestGKEAutoUpgradePausesPastItsWindow moves the Kubernetes Engine
// clock a day forward (#2076).
func gkeNoAutoUpgrade() *containerpb.MaintenancePolicy {
	now := time.Now()
	return &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		MaintenanceExclusions: map[string]*containerpb.TimeWindow{"compat-freeze": {
			StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now.Add(29 * 24 * time.Hour)),
			Options: &containerpb.TimeWindow_MaintenanceExclusionOptions{MaintenanceExclusionOptions: &containerpb.MaintenanceExclusionOptions{
				Scope: containerpb.MaintenanceExclusionOptions_NO_UPGRADES}}}}}}
}

// gkeKubectl runs kubectl through kubeconfig, with stdin.
func gkeKubectl(t *testing.T, kubeconfig, stdin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gkeDeployProbe loads the event probe into the cluster's nodes and runs
// it as a two-replica Deployment behind a Service, with a
// PodDisruptionBudget that keeps one serving while nodes are drained.
func gkeDeployProbe(ctx context.Context, t *testing.T, instance, clusterName, kubeconfig string) {
	t.Helper()
	loadEventProbe(ctx, t)
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", gke.KindName(instance, clusterName)).CombinedOutput(); err != nil {
		t.Fatalf("load the probe into the GKE cluster: %v\n%s", err, b)
	}
	gkeKubectl(t, kubeconfig, `apiVersion: v1
kind: Namespace
metadata: {name: upgrade}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: probe, namespace: upgrade}
spec:
  replicas: 2
  selector: {matchLabels: {app: probe}}
  template:
    metadata: {labels: {app: probe}}
    spec:
      terminationGracePeriodSeconds: 15
      containers:
      - name: probe
        image: `+eventProbeImage+`
        imagePullPolicy: Never
        env: [{name: PORT, value: "8080"}]
        ports: [{containerPort: 8080}]
        readinessProbe: {httpGet: {path: /events, port: 8080}, periodSeconds: 1}
        lifecycle: {preStop: {sleep: {seconds: 5}}}
---
apiVersion: v1
kind: Service
metadata: {name: probe, namespace: upgrade}
spec:
  selector: {app: probe}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: probe, namespace: upgrade}
spec:
  minAvailable: 1
  selector: {matchLabels: {app: probe}}
`, "apply", "-f", "-")
	gkeKubectl(t, kubeconfig, "", "-n", "upgrade", "rollout", "status", "deployment/probe", "--timeout=180s")
}

// gkeProbeServes is one request to the probe through the API server's
// service proxy.
func gkeProbeServes(kubeconfig string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "--raw",
		"/api/v1/namespaces/upgrade/services/probe:80/proxy/events").CombinedOutput()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "[") && !strings.HasPrefix(strings.TrimSpace(string(out)), "null") {
		return &probeErr{string(out)}
	}
	return nil
}

type probeErr struct{ body string }

func (e *probeErr) Error() string { return "unexpected probe answer: " + e.body }

// gkeKubeletVersions is each node's kubelet version, by node.
func gkeKubeletVersions(t *testing.T, kubeconfig, selector string) map[string]string {
	t.Helper()
	out := gkeKubectl(t, kubeconfig, "", "get", "nodes", "-l", selector, "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.status.nodeInfo.kubeletVersion}{"\n"}{end}`)
	m := map[string]string{}
	for _, l := range strings.Fields(out) {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	return m
}

// kubeOf is a GKE version's Kubernetes one: 1.36.4-gke.0 is v1.36.4.
func kubeOf(v string) string {
	if i := strings.Index(v, "-"); i >= 0 {
		v = v[:i]
	}
	return "v" + v
}

// TestGKEUpgradeMasterThenNodePool (#2069), through the official Go
// client, as Google's "Manually upgrading a cluster or node pool"
// documents it: a Standard zonal cluster is made on the older of the two
// served versions (N), with a "no upgrades" maintenance exclusion, which a
// manual upgrade ignores; a two-replica Deployment with a
// PodDisruptionBudget serves behind a Service. The node pool cannot be
// upgraded past the control plane; UpdateMaster upgrades the control
// plane to N+1 (UPGRADE_MASTER, the cluster RECONCILING meanwhile), with
// the workload's pods untouched; UpdateNodePool then surges the pool's
// nodes onto N+1 (UPGRADE_NODES, with progress), while the workload keeps
// answering through the API server; every node reports N+1 afterwards,
// and the cluster's versions say so.
// covers: google.container.v1.ClusterManager/UpdateMaster, google.container.v1.ClusterManager/SetMaintenancePolicy
func TestGKEUpgradeMasterThenNodePool(t *testing.T) {
	h := New(t)
	g, _ := gkeClients(t, h)
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the probe must be loaded into the cluster")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	project, zone := h.Project(), "us-central1-c"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/ct-gke-up"
	older, newer, cfg := gkeUpgradeVersions(t, g, parent)
	for _, ch := range cfg.GetChannels() {
		if !strings.Contains(strings.Join(ch.GetValidVersions(), ","), older) || ch.GetDefaultVersion() != newer {
			t.Errorf("channel %s = %v", ch.GetChannel(), ch)
		}
	}

	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "ct-gke-up", InitialClusterVersion: older, MaintenancePolicy: gkeNoAutoUpgrade(),
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}},
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster on %s: %v", older, err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	gkeWait(t, g, project, zone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if c.GetCurrentMasterVersion() != older || c.GetCurrentNodeVersion() != older || c.GetNodePools()[0].GetVersion() != older {
		t.Fatalf("made on %s/%s/%s, want %s", c.GetCurrentMasterVersion(), c.GetCurrentNodeVersion(), c.GetNodePools()[0].GetVersion(), older)
	}
	if us := c.GetNodePools()[0].GetUpgradeSettings(); us.GetMaxSurge() != 1 || us.GetStrategy() != containerpb.NodePoolUpdateStrategy_SURGE {
		t.Errorf("default upgradeSettings = %v", us)
	}
	if c.GetMaintenancePolicy().GetResourceVersion() == "" {
		t.Errorf("maintenancePolicy has no resourceVersion: %v", c.GetMaintenancePolicy())
	}
	kc := gkeKubeconfig(t, c)
	pool := "cloud.google.com/gke-nodepool=default-pool"
	for n, v := range gkeKubeletVersions(t, kc, pool) {
		if v != kubeOf(older) {
			t.Errorf("node %s runs %s before the upgrade, want %s", n, v, kubeOf(older))
		}
	}
	gkeDeployProbe(ctx, t, instance, name, kc)
	if err := gkeProbeServes(kc); err != nil {
		t.Fatalf("the probe does not serve: %v", err)
	}
	podsBefore := gkeKubectl(t, kc, "", "-n", "upgrade", "get", "pods", "-l", "app=probe", "-o", "jsonpath={.items[*].metadata.uid}")

	// Nodes are never newer than the control plane.
	_, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: newer, ImageType: "COS_CONTAINERD"})
	wantPrivateCACode(t, "nodes before the control plane", err, codes.InvalidArgument)

	// The maintenance policy is kept with its resource version.
	op, err = g.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name, MaintenancePolicy: &containerpb.MaintenancePolicy{
		ResourceVersion: c.GetMaintenancePolicy().GetResourceVersion(), Window: gkeNoAutoUpgrade().GetWindow()}})
	if err != nil {
		t.Fatalf("SetMaintenancePolicy: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name, MaintenancePolicy: &containerpb.MaintenancePolicy{
		ResourceVersion: c.GetMaintenancePolicy().GetResourceVersion()}})
	wantPrivateCACode(t, "a stale resource version", err, codes.FailedPrecondition)

	// The control plane, to N+1.
	op, err = g.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name, MasterVersion: newer})
	if err != nil {
		t.Fatalf("UpdateMaster: %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_MASTER {
		t.Errorf("UpdateMaster's operation type = %s", op.GetOperationType())
	}
	if c, _ := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); c.GetStatus() != containerpb.Cluster_RECONCILING {
		t.Errorf("status while the control plane upgrades = %s", c.GetStatus())
	}
	_, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: older, ImageType: "UBUNTU_CONTAINERD"})
	wantPrivateCACode(t, "a second operation", err, codes.FailedPrecondition)
	gkeWait(t, g, project, zone, op)
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentMasterVersion() != newer || c.GetCurrentNodeVersion() != older || c.GetStatus() != containerpb.Cluster_RUNNING {
		t.Errorf("after UpdateMaster: master %s, nodes %s, %s", c.GetCurrentMasterVersion(), c.GetCurrentNodeVersion(), c.GetStatus())
	}
	if v := gkeKubectl(t, kc, "", "get", "--raw", "/version"); !strings.Contains(v, `"gitVersion": "`+kubeOf(newer)+`"`) {
		t.Errorf("the API server after UpdateMaster is %s, want %s", v, kubeOf(newer))
	}
	if after := gkeKubectl(t, kc, "", "-n", "upgrade", "get", "pods", "-l", "app=probe", "-o", "jsonpath={.items[*].metadata.uid}"); after != podsBefore {
		t.Errorf("the control plane's upgrade replaced the workload's pods: %s, then %s", podsBefore, after)
	}
	if err := gkeProbeServes(kc); err != nil {
		t.Errorf("the probe does not serve after UpdateMaster: %v", err)
	}

	// The node pool, to N+1, surge, while the probe is asked every second.
	var asked, failed atomic.Int32
	var lastErr atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			asked.Add(1)
			// One retry: a request in flight when an endpoint goes away.
			if err := gkeProbeServes(kc); err != nil {
				if err := gkeProbeServes(kc); err != nil {
					failed.Add(1)
					lastErr.Store(err.Error())
				}
			}
		}
	}()
	op, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: "-", ImageType: "COS_CONTAINERD"})
	if err != nil {
		close(stop)
		t.Fatalf("UpdateNodePool: %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("UpdateNodePool's operation type = %s", op.GetOperationType())
	}
	done := gkeWait(t, g, project, zone, op)
	close(stop)
	wg.Wait()
	if failed.Load() > 0 {
		t.Errorf("the workload stopped serving %d of %d times during the node upgrade (last: %v)", failed.Load(), asked.Load(), lastErr.Load())
	}
	if asked.Load() < 5 {
		t.Errorf("the probe was asked only %d times", asked.Load())
	}
	var total, nodesDone int64
	for _, m := range done.GetProgress().GetMetrics() {
		switch m.GetName() {
		case "NODES_TOTAL":
			total = m.GetIntValue()
		case "NODES_DONE":
			nodesDone = m.GetIntValue()
		}
	}
	if total != 1 || nodesDone != 1 {
		t.Errorf("node upgrade progress = %v", done.GetProgress())
	}
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentNodeVersion() != newer || c.GetNodePools()[0].GetVersion() != newer || c.GetNodePools()[0].GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("after UpdateNodePool: nodes %s, pool %v", c.GetCurrentNodeVersion(), c.GetNodePools()[0])
	}
	vs := gkeKubeletVersions(t, kc, pool)
	if len(vs) != 1 {
		t.Errorf("default-pool's nodes after the upgrade = %v, want 1", vs)
	}
	for n, v := range vs {
		if v != kubeOf(newer) {
			t.Errorf("node %s runs %s after the upgrade, want %s", n, v, kubeOf(newer))
		}
	}
	gkeKubectl(t, kc, "", "-n", "upgrade", "rollout", "status", "deployment/probe", "--timeout=120s")
	if err := gkeProbeServes(kc); err != nil {
		t.Errorf("the probe does not serve after the upgrade: %v", err)
	}
	ops, err := g.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[containerpb.Operation_Type]bool{}
	for _, o := range ops.GetOperations() {
		if strings.HasSuffix(o.GetTargetLink(), "/clusters/ct-gke-up") || strings.Contains(o.GetTargetLink(), "/clusters/ct-gke-up/") {
			seen[o.GetOperationType()] = true
		}
	}
	if !seen[containerpb.Operation_UPGRADE_MASTER] || !seen[containerpb.Operation_UPGRADE_NODES] {
		t.Errorf("ListOperations has no upgrade operations: %v", seen)
	}
}

// TestGcloudContainerClustersUpgrade (#2069): `gcloud container clusters
// create --cluster-version` on the older served version with a maintenance
// window, `clusters update --add-maintenance-exclusion-*`, then `gcloud
// container clusters upgrade --master --cluster-version` and `gcloud
// container clusters upgrade --node-pool`, as Google's upgrading-a-cluster
// page gives them; the cluster's versions and its node's kubelet follow.
func TestGcloudContainerClustersUpgrade(t *testing.T) {
	h := New(t)
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
	const zone, cluster = "us-central1-f", "gc-gke-up"
	older, newer, _ := gkeUpgradeVersions(t, g, "projects/"+h.Project()+"/locations/"+zone)
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name,
			"CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true", "KUBECONFIG="+kubeconfig)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	full := "projects/" + h.Project() + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, h.Project(), zone, op)
		}
	})
	// A daily window that starts in 2030, so no automatic upgrade races
	// the manual ones, which ignore windows and exclusions; then a "no
	// upgrades" exclusion over the test, through clusters update
	// (SetMaintenancePolicy).
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate",
		"--cluster-version", older, "--release-channel", "regular",
		"--maintenance-window-start", "2030-01-01T00:00:00Z", "--maintenance-window-end", "2030-01-01T04:00:00Z",
		"--maintenance-window-recurrence", "FREQ=DAILY")
	start := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	end := time.Now().UTC().Add(29 * 24 * time.Hour).Format(time.RFC3339)
	gc("container", "clusters", "update", cluster, "--zone", zone,
		"--add-maintenance-exclusion-name", "compat-freeze", "--add-maintenance-exclusion-start", start,
		"--add-maintenance-exclusion-end", end, "--add-maintenance-exclusion-scope", "no_upgrades")
	if got := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=json(maintenancePolicy)"); !strings.Contains(got, "compat-freeze") || !strings.Contains(got, "FREQ=DAILY") {
		t.Errorf("maintenancePolicy after update:\n%s", got)
	}
	if got := strings.Fields(gc("container", "clusters", "describe", cluster, "--zone", zone,
		"--format=value(currentMasterVersion,currentNodeVersion)")); len(got) != 2 || got[0] != older || got[1] != older {
		t.Fatalf("versions after create = %v, want %s", got, older)
	}
	gc("container", "clusters", "upgrade", cluster, "--zone", zone, "--master", "--cluster-version", newer)
	if got := strings.TrimSpace(gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(currentMasterVersion)")); got != newer {
		t.Errorf("currentMasterVersion after upgrade --master = %q, want %s", got, newer)
	}
	gc("container", "clusters", "upgrade", cluster, "--zone", zone, "--node-pool", "default-pool")
	if got := strings.TrimSpace(gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(version)")); got != newer {
		t.Errorf("default-pool's version after upgrade --node-pool = %q, want %s", got, newer)
	}
	if got := gc("container", "operations", "list", "--zone", zone, "--format=value(operationType)"); !strings.Contains(got, "UPGRADE_MASTER") || !strings.Contains(got, "UPGRADE_NODES") {
		t.Errorf("operations list:\n%s", got)
	}
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)
	for n, v := range gkeKubeletVersions(t, kubeconfig, "cloud.google.com/gke-nodepool=default-pool") {
		if v != kubeOf(newer) {
			t.Errorf("node %s runs %s, want %s", n, v, kubeOf(newer))
		}
	}
}
