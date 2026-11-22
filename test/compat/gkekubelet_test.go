//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// gkeKubeletConfigz is the running configuration of node's kubelet, read
// through the API server's node proxy (/configz).
func gkeKubeletConfigz(t *testing.T, kubeconfig, node string) map[string]any {
	t.Helper()
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/configz").Output()
	if err != nil {
		t.Fatalf("read node %s's kubelet configuration: %v", node, err)
	}
	var z struct {
		KubeletConfig map[string]any `json:"kubeletconfig"`
	}
	if err := json.Unmarshal(out, &z); err != nil {
		t.Fatalf("node %s's configz: %v\n%s", node, err, out)
	}
	return z.KubeletConfig
}

// gkeWantKubelet checks node's running kubelet fields.
func gkeWantKubelet(t *testing.T, kubeconfig, node string, want map[string]any) {
	t.Helper()
	got := gkeKubeletConfigz(t, kubeconfig, node)
	for k, w := range want {
		g, _ := json.Marshal(got[k])
		ws, _ := json.Marshal(w)
		if string(g) != string(ws) {
			t.Errorf("node %s's kubelet %s = %s, want %s", node, k, g, ws)
		}
	}
}

func i32p(n int32) *int32 { return &n }

// TestGKENodeKubeletConfig (#2100): a node pool's kubelet_config is what
// its nodes' kubelets run with. A pool made with one (CPU manager static,
// a topology manager policy, a pod PID limit, log rotation, a soft memory
// eviction with its grace period, parallel image pulls, a custom CFS
// period, graceful shutdown) has kubelets reporting those settings
// (/configz); UpdateNodePool with another (memory manager Static, an
// image maximum age, unsafe net.* sysctls allowed) is an UPGRADE_NODES
// operation that re-creates the node, whose kubelet then runs the new
// settings, and a pod setting the unsafe sysctl net.core.somaxconn runs on
// it. A setting out of GKE's documented range is INVALID_ARGUMENT.
//
// covers: google.container.v1.ClusterManager/UpdateNodePool, google.container.v1.ClusterManager/CreateNodePool
func TestGKENodeKubeletConfig(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-c", "kubelet-2100"
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
	kc := gkeKubeconfig(t, c)
	_, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "bad", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{KubeletConfig: &containerpb.NodeKubeletConfig{ContainerLogMaxSize: "600Mi"}}}})
	wantPrivateCACode(t, "a 600Mi containerLogMaxSize", err, codes.InvalidArgument)

	pool := name + "/nodePools/tuned"
	op, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "tuned", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{KubeletConfig: &containerpb.NodeKubeletConfig{
			CpuManagerPolicy: "static", TopologyManager: &containerpb.TopologyManager{Policy: "best-effort", Scope: "pod"},
			PodPidsLimit: 4096, ContainerLogMaxSize: "20Mi", ContainerLogMaxFiles: 3, CpuCfsQuotaPeriod: "50ms", CpuCfsQuota: wrapperspb.Bool(true),
			EvictionSoft:            &containerpb.EvictionSignals{MemoryAvailable: "200Mi"},
			EvictionSoftGracePeriod: &containerpb.EvictionGracePeriod{MemoryAvailable: "30s"},
			MaxParallelImagePulls:   3, ShutdownGracePeriodSeconds: i32p(30), ShutdownGracePeriodCriticalPodsSeconds: i32p(10)}}}})
	if err != nil {
		t.Fatalf("CreateNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	nodes := gkeNodes(t, kc, "tuned")
	if len(nodes) != 1 {
		t.Fatalf("tuned's nodes = %v", nodes)
	}
	gkeWantKubelet(t, kc, nodes[0], map[string]any{"cpuManagerPolicy": "static", "topologyManagerPolicy": "best-effort", "topologyManagerScope": "pod",
		"podPidsLimit": 4096, "containerLogMaxSize": "20Mi", "containerLogMaxFiles": 3, "cpuCFSQuotaPeriod": "50ms",
		"evictionSoft": map[string]any{"memory.available": "200Mi"}, "evictionSoftGracePeriod": map[string]any{"memory.available": "30s"},
		"maxParallelImagePulls": 3, "serializeImagePulls": false, "shutdownGracePeriod": "30s", "shutdownGracePeriodCriticalPods": "10s"})

	_, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool,
		KubeletConfig: &containerpb.NodeKubeletConfig{AllowedUnsafeSysctls: []string{"vm.swappiness"}}})
	wantPrivateCACode(t, "an unsafe sysctl outside GKE's groups", err, codes.InvalidArgument)
	op, err = r.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, KubeletConfig: &containerpb.NodeKubeletConfig{
		MemoryManager: &containerpb.MemoryManager{Policy: "Static"}, PodPidsLimit: 8192, ImageMaximumGcAge: "24h",
		AllowedUnsafeSysctls: []string{"net.*"}}})
	if err != nil {
		t.Fatalf("UpdateNodePool (kubelet_config): %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("the kubelet_config update's operation type = %s", op.GetOperationType())
	}
	gkeWait(t, g, project, zone, op)
	after := gkeNodes(t, kc, "tuned")
	if len(after) != 1 || after[0] == nodes[0] {
		t.Fatalf("tuned's nodes after the kubelet_config update = %v, want %s replaced", after, nodes[0])
	}
	gkeWantKubelet(t, kc, after[0], map[string]any{"memoryManagerPolicy": "Static", "podPidsLimit": 8192, "imageMaximumGCAge": "24h0m0s",
		"allowedUnsafeSysctls": []string{"net.*"}, "cpuManagerPolicy": "none"})
	np, err := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil || np.GetConfig().GetKubeletConfig().GetPodPidsLimit() != 8192 || np.GetConfig().GetKubeletConfig().GetCpuManagerPolicy() != "" ||
		np.GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("the pool after the update: %v %s %v", np.GetConfig().GetKubeletConfig(), np.GetStatus(), err)
	}
	gkeKubectl(t, kc, `apiVersion: v1
kind: Pod
metadata: {name: somaxconn, namespace: default}
spec:
  nodeSelector: {cloud.google.com/gke-nodepool: tuned}
  securityContext:
    sysctls: [{name: net.core.somaxconn, value: "1024"}]
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.10
    imagePullPolicy: Never
`, "apply", "-f", "-")
	if !gkeEventually(2*time.Minute, func() bool { return strings.HasPrefix(gkePodPhase(t, kc, "somaxconn"), "Running ") }) {
		t.Errorf("a pod setting the allowed unsafe sysctl net.core.somaxconn is %q, want Running", gkePodPhase(t, kc, "somaxconn"))
	}
}

// TestGcloudContainerNodePoolsSystemConfig (#2100): `gcloud container
// node-pools update --system-config-from-file` with a kubeletConfig
// re-creates the pool's node with those kubelet settings, which describe
// reads back.
func TestGcloudContainerNodePoolsSystemConfig(t *testing.T) {
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
	const zone, cluster = "us-central1-a", "gc-kubelet-2100"
	project := h.Project()
	gdir := t.TempDir()
	cfgName := gcloudSetup(t, cli, gdir)
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", project, "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfgName)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	full := "projects/" + project + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate")
	c, err := g.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: full})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)
	before := gkeNodes(t, kc, "default-pool")
	file := filepath.Join(t.TempDir(), "system-config.yaml")
	if err := os.WriteFile(file, []byte("kubeletConfig:\n  podPidsLimit: 2048\n  containerLogMaxSize: 30Mi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gc("container", "node-pools", "update", "default-pool", "--cluster", cluster, "--zone", zone, "--system-config-from-file="+file)
	after := gkeNodes(t, kc, "default-pool")
	if len(after) != 1 || len(before) != 1 || after[0] == before[0] {
		t.Fatalf("default-pool's nodes %v then %v, want the node re-created", before, after)
	}
	gkeWantKubelet(t, kc, after[0], map[string]any{"podPidsLimit": 2048, "containerLogMaxSize": "30Mi"})
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone,
		"--format=value(config.kubeletConfig.podPidsLimit)"); got != "2048" {
		t.Errorf("describe's podPidsLimit = %q", got)
	}
}
