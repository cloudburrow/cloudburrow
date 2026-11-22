//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

// gkeCordoned is whether each node of selector is cordoned
// (spec.unschedulable), by node.
func gkeCordoned(t *testing.T, kubeconfig, selector string) map[string]bool {
	t.Helper()
	out := gkeKubectl(t, kubeconfig, "", "get", "nodes", "-l", selector, "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.spec.unschedulable}{"\n"}{end}`)
	m := map[string]bool{}
	for _, l := range strings.Fields(out) {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v == "true"
	}
	return m
}

// gkeWaitPhase polls a node pool until its blue-green upgrade is in phase
// ph, and returns the pool.
func gkeWaitPhase(t *testing.T, g *container.ClusterManagerClient, pool string, ph containerpb.NodePool_UpdateInfo_BlueGreenInfo_Phase) *containerpb.NodePool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Minute)
	var np *containerpb.NodePool
	for time.Now().Before(deadline) {
		var err error
		np, err = g.GetNodePool(context.Background(), &containerpb.GetNodePoolRequest{Name: pool})
		if err != nil {
			t.Fatalf("GetNodePool: %v", err)
		}
		if np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() == ph {
			return np
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("node pool %s never reached %s: %v", pool, ph, np.GetUpdateInfo())
	return nil
}

// gkeUpgradeOp is the running UPGRADE_NODES operation on a node pool.
func gkeUpgradeOp(t *testing.T, g *container.ClusterManagerClient, parent, cluster, pool string) *containerpb.Operation {
	t.Helper()
	ops, err := g.ListOperations(context.Background(), &containerpb.ListOperationsRequest{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops.GetOperations() {
		if strings.HasSuffix(o.GetTargetLink(), "/clusters/"+cluster+"/nodePools/"+pool) && o.GetOperationType() == containerpb.Operation_UPGRADE_NODES &&
			o.GetStatus() != containerpb.Operation_DONE {
			return o
		}
	}
	t.Fatalf("no running upgrade of %s/%s", cluster, pool)
	return nil
}

// TestGKEBlueGreenCompleteNodePoolUpgrade (#2100), through the official Go
// client over gRPC and REST, as Google's node-pool-upgrade-strategies
// pages document a blue-green upgrade ("Complete upgrade is a method used
// to skip the remaining node pool soaking phase", gcloud's
// complete-upgrade reference): a cluster's control plane runs N+1 and its
// two-node default-pool N. CompleteNodePoolUpgrade with no upgrade is
// FAILED_PRECONDITION. UpdateNodePool to N+1 with BLUE_GREEN upgrade
// settings (one node per batch, an hour's node pool soak) reaches
// NODE_POOL_SOAKING with green_pool_version N+1: four nodes, the two
// green ones on N+1 and schedulable, the two blue ones on N and cordoned,
// and the operation still running. CompleteNodePoolUpgrade (over REST)
// ends the soak: the operation is DONE with no error well inside the
// hour, the blue nodes are deleted, both nodes left run N+1, the pool's
// version is N+1, RUNNING, with no update_info; a second
// CompleteNodePoolUpgrade is FAILED_PRECONDITION.
// covers: google.container.v1.ClusterManager/CompleteNodePoolUpgrade, google.container.v1.ClusterManager/UpdateNodePool
func TestGKEBlueGreenCompleteNodePoolUpgrade(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	project, zone := h.Project(), "us-central1-c"
	parent := "projects/" + project + "/locations/" + zone
	older, newer, _ := gkeUpgradeVersions(t, g, parent)
	c := gkeOlderPoolCluster(ctx, t, g, project, zone, "ct-gke-bg", older, newer, 2, gkeNoAutoUpgrade())
	pool := parent + "/clusters/ct-gke-bg/nodePools/default-pool"
	kc := gkeKubeconfig(t, c)
	sel := "cloud.google.com/gke-nodepool=default-pool"

	err := g.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool})
	wantPrivateCACode(t, "completing with no upgrade", err, codes.FailedPrecondition)

	bg := containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	start := time.Now()
	op, err := g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newer, ImageType: "COS_CONTAINERD",
		UpgradeSettings: &containerpb.NodePool_UpgradeSettings{Strategy: &bg, MaxSurge: 1, BlueGreenSettings: &containerpb.BlueGreenSettings{
			RolloutPolicy: &containerpb.BlueGreenSettings_StandardRolloutPolicy_{StandardRolloutPolicy: &containerpb.BlueGreenSettings_StandardRolloutPolicy{
				UpdateBatchSize:   &containerpb.BlueGreenSettings_StandardRolloutPolicy_BatchNodeCount{BatchNodeCount: 1},
				BatchSoakDuration: durationpb.New(0)}},
			NodePoolSoakDuration: durationpb.New(time.Hour)}}})
	if err != nil {
		t.Fatalf("UpdateNodePool (blue-green): %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("the upgrade's operation type = %s", op.GetOperationType())
	}
	np := gkeWaitPhase(t, g, pool, containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING)
	if v := np.GetUpdateInfo().GetBlueGreenInfo().GetGreenPoolVersion(); v != newer {
		t.Errorf("green_pool_version = %q, want %s", v, newer)
	}
	if np.GetVersion() != older {
		t.Errorf("the soaking pool's version = %s, want %s", np.GetVersion(), older)
	}
	vs := gkeKubeletVersions(t, kc, sel)
	cordoned := gkeCordoned(t, kc, sel)
	if got := countVersions(vs); len(vs) != 4 || got[kubeOf(older)] != 2 || got[kubeOf(newer)] != 2 {
		t.Fatalf("nodes while soaking = %v, want two on each version", vs)
	}
	for n, v := range vs {
		if blue := v == kubeOf(older); cordoned[n] != blue {
			t.Errorf("node %s (%s) cordoned = %v, want %v", n, v, cordoned[n], blue)
		}
	}
	if got, _ := g.GetOperation(ctx, &containerpb.GetOperationRequest{Name: parent + "/operations/" + op.GetName()}); got.GetStatus() != containerpb.Operation_RUNNING {
		t.Errorf("the soaking upgrade's status = %s, want RUNNING", got.GetStatus())
	}

	if err := r.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool}); err != nil {
		t.Fatalf("CompleteNodePoolUpgrade (REST): %v", err)
	}
	done := gkeWaitEnd(t, g, project, zone, op.GetName())
	if done.GetError() != nil {
		t.Fatalf("the completed upgrade ended with %v", done.GetError())
	}
	if time.Since(start) > 45*time.Minute {
		t.Errorf("the upgrade took %v: the soak was not cut short", time.Since(start))
	}
	vs = gkeKubeletVersions(t, kc, sel)
	if got := countVersions(vs); len(vs) != 2 || got[kubeOf(newer)] != 2 {
		t.Errorf("nodes after Complete = %v, want 2 on %s", vs, kubeOf(newer))
	}
	for n, c := range gkeCordoned(t, kc, sel) {
		if c {
			t.Errorf("green node %s is cordoned after the upgrade", n)
		}
	}
	np, err = g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	if np.GetVersion() != newer || np.GetStatus() != containerpb.NodePool_RUNNING || np.GetUpdateInfo() != nil {
		t.Errorf("after Complete: version %s, status %s, update_info %v", np.GetVersion(), np.GetStatus(), np.GetUpdateInfo())
	}
	if np.GetUpgradeSettings().GetStrategy() != bg || np.GetUpgradeSettings().GetBlueGreenSettings().GetNodePoolSoakDuration().AsDuration() != time.Hour {
		t.Errorf("the pool's upgrade settings = %v", np.GetUpgradeSettings())
	}
	err = r.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool})
	wantPrivateCACode(t, "completing a finished upgrade", err, codes.FailedPrecondition)
}

// TestGcloudContainerNodePoolsCompleteUpgrade (#2100): gcloud's own
// commands, as their reference pages give them, on a one-node pool that
// runs N under a control plane on N+1: `node-pools update
// --enable-blue-green-upgrade --node-pool-soak-duration=3600s
// --standard-rollout-policy=batch-node-count=1,batch-soak-duration=0s`
// (an update with neither node_version nor image_type), read back with
// `node-pools describe`; `clusters upgrade --node-pool --cluster-version
// --async` starts the blue-green upgrade; once it soaks, `node-pools
// complete-upgrade` ends it: the operation finishes, the node left runs
// N+1, `describe` reads the pool on N+1, and a second complete-upgrade
// fails.
func TestGcloudContainerNodePoolsCompleteUpgrade(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	const zone, cluster = "us-central1-c", "gc-gke-bg"
	project := h.Project()
	parent := "projects/" + project + "/locations/" + zone
	older, newer, _ := gkeUpgradeVersions(t, g, parent)
	gdir := t.TempDir()
	cfgName := gcloudSetup(t, cli, gdir)
	gc := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", project, "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfgName)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	c := gkeOlderPoolCluster(ctx, t, g, project, zone, cluster, older, newer, 1, gkeNoAutoUpgrade())
	pool := parent + "/clusters/" + cluster + "/nodePools/default-pool"
	kc := gkeKubeconfig(t, c)
	sel := "cloud.google.com/gke-nodepool=default-pool"

	if out, err := gc("container", "node-pools", "update", "default-pool", "--cluster", cluster, "--zone", zone,
		"--enable-blue-green-upgrade", "--node-pool-soak-duration=3600s", "--standard-rollout-policy=batch-node-count=1,batch-soak-duration=0s"); err != nil {
		t.Fatalf("gcloud container node-pools update --enable-blue-green-upgrade: %v\n%s", err, out)
	}
	out, err := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone,
		"--format=value(upgradeSettings.strategy,upgradeSettings.blueGreenSettings.nodePoolSoakDuration,version)")
	if err != nil || strings.Join(strings.Fields(out), " ") != "BLUE_GREEN 3600s "+older {
		t.Errorf("node-pools describe after the update = %q (%v)", out, err)
	}
	if out, err := gc("container", "node-pools", "complete-upgrade", "default-pool", "--cluster", cluster, "--zone", zone); err == nil {
		t.Errorf("complete-upgrade with no upgrade succeeded:\n%s", out)
	}
	if out, err := gc("container", "clusters", "upgrade", cluster, "--node-pool", "default-pool", "--cluster-version", newer, "--zone", zone, "--async"); err != nil {
		t.Fatalf("gcloud container clusters upgrade --node-pool --async: %v\n%s", err, out)
	}
	gkeWaitPhase(t, g, pool, containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING)
	op := gkeUpgradeOp(t, g, parent, cluster, "default-pool")
	if got := countVersions(gkeKubeletVersions(t, kc, sel)); got[kubeOf(older)] != 1 || got[kubeOf(newer)] != 1 {
		t.Fatalf("nodes while soaking = %v, want one blue and one green", got)
	}
	if out, err := gc("container", "node-pools", "complete-upgrade", "default-pool", "--cluster", cluster, "--zone", zone); err != nil {
		t.Fatalf("gcloud container node-pools complete-upgrade: %v\n%s", err, out)
	}
	if done := gkeWaitEnd(t, g, project, zone, op.GetName()); done.GetError() != nil {
		t.Fatalf("the completed upgrade ended with %v", done.GetError())
	}
	vs := gkeKubeletVersions(t, kc, sel)
	if got := countVersions(vs); len(vs) != 1 || got[kubeOf(newer)] != 1 {
		t.Errorf("nodes after complete-upgrade = %v, want 1 on %s", vs, kubeOf(newer))
	}
	if out, err := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(version,status)"); err != nil ||
		strings.Join(strings.Fields(out), " ") != newer+" RUNNING" {
		t.Errorf("node-pools describe after complete-upgrade = %q (%v)", out, err)
	}
	if out, err := gc("container", "node-pools", "complete-upgrade", "default-pool", "--cluster", cluster, "--zone", zone); err == nil {
		t.Errorf("a second complete-upgrade succeeded:\n%s", out)
	}
}
