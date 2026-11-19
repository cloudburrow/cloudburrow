//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// EnvGKE is the host address of the Kubernetes Engine API
// (container.googleapis.com, #1913), gRPC and JSON on one port.
const EnvGKE = "CLOUDBURROW_TEST_GKE"

// gkeClients is the official client over gRPC and over REST.
func gkeClients(t *testing.T, h *Harness) (grpcClient, restClient *container.ClusterManagerClient) {
	t.Helper()
	addr := h.Endpoint(EnvGKE)
	ctx := context.Background()
	g, err := container.NewClusterManagerClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := container.NewClusterManagerRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return g, r
}

// gkeWait polls an operation until it is DONE, failing on its error.
func gkeWait(t *testing.T, c *container.ClusterManagerClient, project, location string, op *containerpb.Operation) *containerpb.Operation {
	t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	name := "projects/" + project + "/locations/" + location + "/operations/" + op.GetName()
	for time.Now().Before(deadline) {
		got, err := c.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatalf("GetOperation %s: %v", name, err)
		}
		if got.GetStatus() == containerpb.Operation_DONE {
			if got.GetError() != nil {
				t.Fatalf("operation %s (%s) failed: %v", got.GetName(), got.GetOperationType(), got.GetError())
			}
			return got
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("operation %s did not finish", name)
	return nil
}

// gkeKubeconfig writes a kubeconfig from a cluster's endpoint and
// certificates, as a client of the API would.
func gkeKubeconfig(t *testing.T, c *containerpb.Cluster) string {
	t.Helper()
	ma := c.GetMasterAuth()
	if ma.GetClusterCaCertificate() == "" || ma.GetClientCertificate() == "" || ma.GetClientKey() == "" {
		t.Fatalf("cluster %s has no CA or client certificate: %v", c.GetName(), ma)
	}
	for _, v := range []string{ma.GetClusterCaCertificate(), ma.GetClientCertificate(), ma.GetClientKey()} {
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			t.Fatalf("masterAuth holds a value that is not base64: %v", err)
		}
	}
	kc := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://` + c.GetEndpoint() + `
    certificate-authority-data: ` + ma.GetClusterCaCertificate() + `
users:
- name: u
  user:
    client-certificate-data: ` + ma.GetClientCertificate() + `
    client-key-data: ` + ma.GetClientKey() + `
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// gkeNodes is `kubectl get nodes` of a pool, through kubeconfig.
func gkeNodes(t *testing.T, kubeconfig, pool string) []string {
	t.Helper()
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "nodes", "-l", "cloud.google.com/gke-nodepool="+pool,
		"-o", "jsonpath={.items[*].metadata.name}").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get nodes: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// TestGKEClustersNodePoolsAndOperations (#1913): the official client, over
// gRPC, makes a Standard zonal cluster, which is a real local Kubernetes
// cluster: kubectl reaches it through the endpoint and certificates the API
// returns, and its nodes are the node pools'. A node pool is created,
// resized up and down (the nodes kubectl sees follow) and deleted; labels
// are set with their fingerprint; the server config names the versions;
// operations are listed. Over REST the same client reads it back, and an
// Autopilot regional cluster is made and refuses node pool changes.
// Validation and Google's error codes are checked on the way.
// covers: google.container.v1.ClusterManager/CreateCluster, google.container.v1.ClusterManager/GetCluster, google.container.v1.ClusterManager/ListClusters, google.container.v1.ClusterManager/DeleteCluster, google.container.v1.ClusterManager/UpdateCluster, google.container.v1.ClusterManager/SetLabels, google.container.v1.ClusterManager/GetServerConfig, google.container.v1.ClusterManager/CreateNodePool, google.container.v1.ClusterManager/GetNodePool, google.container.v1.ClusterManager/ListNodePools, google.container.v1.ClusterManager/SetNodePoolSize, google.container.v1.ClusterManager/UpdateNodePool, google.container.v1.ClusterManager/DeleteNodePool, google.container.v1.ClusterManager/GetOperation, google.container.v1.ClusterManager/ListOperations, google.container.v1.ClusterManager/CancelOperation
func TestGKEClustersNodePoolsAndOperations(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project, zone := h.Project(), "us-central1-a"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/ct-gke-std"

	// Validation, before anything is made.
	_, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "Bad_Name", InitialNodeCount: 1}})
	wantPrivateCACode(t, "a bad name", err, codes.InvalidArgument)
	_, err = g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ct-gke-zero"}})
	wantPrivateCACode(t, "no nodes", err, codes.InvalidArgument)
	_, err = g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ct-gke-ap", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	wantPrivateCACode(t, "a zonal Autopilot cluster", err, codes.InvalidArgument)
	_, err = g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ct-gke-v", InitialNodeCount: 1, InitialClusterVersion: "1.2.3"}})
	wantPrivateCACode(t, "an unserved version", err, codes.InvalidArgument)
	_, err = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/ct-gke-missing"})
	wantPrivateCACode(t, "a missing cluster", err, codes.NotFound)

	cfg, err := g.GetServerConfig(ctx, &containerpb.GetServerConfigRequest{Name: parent})
	if err != nil {
		t.Fatalf("GetServerConfig: %v", err)
	}
	if cfg.GetDefaultClusterVersion() == "" || len(cfg.GetValidMasterVersions()) == 0 || len(cfg.GetChannels()) == 0 {
		t.Fatalf("server config = %v", cfg)
	}

	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "ct-gke-std", InitialNodeCount: 1, ResourceLabels: map[string]string{"team": "ct"},
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	if op.GetOperationType() != containerpb.Operation_CREATE_CLUSTER {
		t.Errorf("create's operation type = %s", op.GetOperationType())
	}
	_, err = g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ct-gke-std", InitialNodeCount: 1}})
	wantPrivateCACode(t, "a duplicate", err, codes.AlreadyExists)
	gkeWait(t, g, project, zone, op)

	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if c.GetStatus() != containerpb.Cluster_RUNNING || c.GetCurrentMasterVersion() != cfg.GetDefaultClusterVersion() ||
		len(c.GetNodePools()) != 1 || c.GetNodePools()[0].GetName() != "default-pool" || c.GetLocation() != zone {
		t.Errorf("cluster = %v", c)
	}
	kc := gkeKubeconfig(t, c)
	if nodes := gkeNodes(t, kc, "default-pool"); len(nodes) != 1 {
		t.Errorf("default-pool's nodes = %v, want 1", nodes)
	}

	// Over REST, the same records.
	l, err := r.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		t.Fatalf("ListClusters over REST: %v", err)
	}
	found := false
	for _, x := range l.GetClusters() {
		found = found || x.GetName() == "ct-gke-std"
	}
	if !found {
		t.Errorf("ListClusters does not list ct-gke-std: %v", l)
	}

	// Node pools: create, resize up and down, update, delete.
	pool := name + "/nodePools/ct-pool"
	op, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "ct-pool", InitialNodeCount: 1}})
	if err != nil {
		t.Fatalf("CreateNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	op, err = g.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: pool, NodeCount: 2})
	if err != nil {
		t.Fatalf("SetNodePoolSize 2: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	if nodes := gkeNodes(t, kc, "ct-pool"); len(nodes) != 2 {
		t.Errorf("ct-pool's nodes after resizing to 2 = %v", nodes)
	}
	op, err = g.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: pool, NodeCount: 1})
	if err != nil {
		t.Fatalf("SetNodePoolSize 1: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	if nodes := gkeNodes(t, kc, "ct-pool"); len(nodes) != 1 {
		t.Errorf("ct-pool's nodes after resizing to 1 = %v", nodes)
	}
	np, err := r.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil || np.GetInitialNodeCount() != 1 || np.GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("GetNodePool over REST = %v, %v", np, err)
	}
	op, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: "-", ImageType: "UBUNTU_CONTAINERD"})
	if err != nil {
		t.Fatalf("UpdateNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	pools, err := g.ListNodePools(ctx, &containerpb.ListNodePoolsRequest{Parent: name})
	if err != nil || len(pools.GetNodePools()) != 2 {
		t.Errorf("ListNodePools = %v, %v", pools, err)
	}
	op, err = g.DeleteNodePool(ctx, &containerpb.DeleteNodePoolRequest{Name: pool})
	if err != nil {
		t.Fatalf("DeleteNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	if nodes := gkeNodes(t, kc, "ct-pool"); len(nodes) != 0 {
		t.Errorf("ct-pool's nodes after delete = %v", nodes)
	}

	// Labels, with the fingerprint of the ones the cluster has.
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	_, err = g.SetLabels(ctx, &containerpb.SetLabelsRequest{Name: name, ResourceLabels: map[string]string{"team": "x"}, LabelFingerprint: "stale"})
	wantPrivateCACode(t, "a stale fingerprint", err, codes.FailedPrecondition)
	op, err = g.SetLabels(ctx, &containerpb.SetLabelsRequest{Name: name, ResourceLabels: map[string]string{"team": "ct", "env": "dev"}, LabelFingerprint: c.GetLabelFingerprint()})
	if err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredReleaseChannel: &containerpb.ReleaseChannel{Channel: containerpb.ReleaseChannel_STABLE}}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetResourceLabels()["env"] != "dev" || c.GetReleaseChannel().GetChannel() != containerpb.ReleaseChannel_STABLE {
		t.Errorf("after SetLabels and UpdateCluster: labels %v, channel %v", c.GetResourceLabels(), c.GetReleaseChannel())
	}

	ops, err := g.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: parent})
	if err != nil || len(ops.GetOperations()) < 7 {
		t.Errorf("ListOperations = %d, %v", len(ops.GetOperations()), err)
	}
	err = g.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: parent + "/operations/" + op.GetName()})
	wantPrivateCACode(t, "cancelling a done operation", err, codes.FailedPrecondition)

	// Autopilot, over REST: a regional cluster whose nodes Google manages.
	region := "us-central1"
	apName := "projects/" + project + "/locations/" + region + "/clusters/ct-gke-auto"
	op, err = r.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + region,
		Cluster: &containerpb.Cluster{Name: "ct-gke-auto", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	if err != nil {
		t.Fatalf("CreateCluster Autopilot over REST: %v", err)
	}
	t.Cleanup(func() {
		if op, err := r.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: apName}); err == nil {
			gkeWait(t, r, project, region, op)
		}
	})
	gkeWait(t, r, project, region, op)
	ap, err := r.GetCluster(ctx, &containerpb.GetClusterRequest{Name: apName})
	if err != nil || !ap.GetAutopilot().GetEnabled() || ap.GetStatus() != containerpb.Cluster_RUNNING || len(ap.GetLocations()) != 3 {
		t.Errorf("Autopilot cluster = %v, %v", ap, err)
	}
	_, err = r.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: apName, NodePool: &containerpb.NodePool{Name: "p", InitialNodeCount: 1}})
	wantPrivateCACode(t, "a node pool on Autopilot", err, codes.FailedPrecondition)

	// Delete, and the cluster is gone.
	op, err = g.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	wantPrivateCACode(t, "a deleted cluster", err, codes.NotFound)
}

// TestGcloudContainerClusters (#1913): gcloud container clusters, configured
// by `cloudburrow gcloud-setup` (its container override), creates a cluster,
// lists and describes it, and get-credentials writes a kubeconfig through
// which kubectl reaches the local cluster's nodes; then it deletes it.
// gcloud's kubeconfig carries the cluster's client certificate
// (container/use_client_certificate), since the exec plugin it writes
// otherwise, gke-gcloud-auth-plugin, mints Google tokens.
// covers: google.container.v1.ClusterManager/CreateCluster, google.container.v1.ClusterManager/ListClusters, google.container.v1.ClusterManager/GetCluster, google.container.v1.ClusterManager/DeleteCluster, google.container.v1.ClusterManager/GetOperation
func TestGcloudContainerClusters(t *testing.T) {
	h := New(t)
	g, _ := gkeClients(t, h) // skips without CLOUDBURROW_TEST_GKE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "container = http://") {
		t.Fatalf("the configuration has no container override:\n%s", conf)
	}
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
	const zone, cluster = "us-central1-b", "gc-gke-c1"
	full := "projects/" + h.Project() + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, h.Project(), zone, op)
		}
	})
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate")
	if got := gc("container", "clusters", "list", "--zone", zone, "--format=value(name,status)"); !strings.Contains(got, cluster) || !strings.Contains(got, "RUNNING") {
		t.Errorf("clusters list:\n%s", got)
	}
	if got := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(currentNodeCount)"); strings.TrimSpace(got) != "1" {
		t.Errorf("clusters describe currentNodeCount = %q", got)
	}
	_ = os.Remove(kubeconfig)
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "nodes", "-l", "cloud.google.com/gke-nodepool=default-pool",
		"-o", "jsonpath={.items[*].metadata.name}").CombinedOutput()
	if err != nil || len(strings.Fields(string(out))) != 1 {
		t.Fatalf("kubectl through gcloud's kubeconfig: %v\n%s", err, out)
	}
	ctxName, _ := exec.Command("kubectl", "--kubeconfig", kubeconfig, "config", "current-context").Output()
	if want := "gke_" + h.Project() + "_" + zone + "_" + cluster; strings.TrimSpace(string(ctxName)) != want {
		t.Errorf("kubectl context = %q, want %q", ctxName, want)
	}
	gc("container", "clusters", "delete", cluster, "--zone", zone)
	if got := gc("container", "clusters", "list", "--zone", zone, "--format=value(name)"); strings.Contains(got, cluster) {
		t.Errorf("clusters list after delete:\n%s", got)
	}
}
