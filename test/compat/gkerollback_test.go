//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// gkeClockAdvance moves the Kubernetes Engine clock d forward (0 reads it)
// through CloudBurrow's extension on the API's port, POST
// /cloudburrow/v1/clock:advance (#2076; Google has no such call), and
// returns the clock's time.
func gkeClockAdvance(t *testing.T, h *Harness, d time.Duration) time.Time {
	t.Helper()
	addr := h.Endpoint(EnvGKE)
	if addr == "" {
		t.Fatal("no Kubernetes Engine endpoint")
	}
	res, err := http.Post("http://"+strings.TrimPrefix(addr, "http://")+"/cloudburrow/v1/clock:advance", "application/json",
		strings.NewReader(fmt.Sprintf(`{"duration": "%ds"}`, int64(d/time.Second))))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("clock:advance: %d %v", res.StatusCode, err)
	}
	now, err := time.Parse(time.RFC3339Nano, out.Now)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// gkeWaitEnd polls an operation until it is DONE and returns it, error
// or not.
func gkeWaitEnd(t *testing.T, g *container.ClusterManagerClient, project, location, op string) *containerpb.Operation {
	t.Helper()
	name := "projects/" + project + "/locations/" + location + "/operations/" + op
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		got, err := g.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatalf("GetOperation %s: %v", name, err)
		}
		if got.GetStatus() == containerpb.Operation_DONE {
			return got
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("operation %s did not finish", name)
	return nil
}

// gkeOlderPoolCluster makes a Standard zonal cluster whose control plane
// runs newer and whose default-pool (n nodes) runs older, with policy.
func gkeOlderPoolCluster(ctx context.Context, t *testing.T, g *container.ClusterManagerClient, project, zone, cluster, older, newer string, n int32,
	policy *containerpb.MaintenancePolicy) *containerpb.Cluster {
	t.Helper()
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/" + cluster
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: cluster, InitialClusterVersion: newer, MaintenancePolicy: policy,
		NodePools: []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: n, Version: older,
			Management: &containerpb.NodeManagement{AutoUpgrade: true, AutoRepair: true}}},
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
	gkeWait(t, g, project, zone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if c.GetCurrentMasterVersion() != newer || c.GetNodePools()[0].GetVersion() != older || c.GetCurrentNodeVersion() != older {
		t.Fatalf("made with master %s, pool %s, nodes %s; want %s and %s", c.GetCurrentMasterVersion(), c.GetNodePools()[0].GetVersion(),
			c.GetCurrentNodeVersion(), newer, older)
	}
	return c
}

// countVersions is how many of the nodes run each kubelet version.
func countVersions(vs map[string]string) map[string]int {
	out := map[string]int{}
	for _, v := range vs {
		out[v]++
	}
	return out
}

// TestGKERollbackNodePoolUpgrade (#2076), through the official Go client,
// as Google's upgrading-a-cluster page documents it ("Roll back a node
// pool upgrade": after an upgrade is cancelled, fails or is incomplete):
// a cluster's control plane runs N+1 and its two-node default-pool N, with
// a two-replica Deployment and a PodDisruptionBudget serving. A completed
// pool has nothing to roll back (FAILED_PRECONDITION); UpdateNodePool to
// N+1 is cancelled at once (CancelOperation): ABORTING, then DONE with
// CANCELLED once the node that had started is upgraded, and the other
// node untouched; a second cancel is FAILED_PRECONDITION.
// RollbackNodePoolUpgrade (respect_pdb) is an UPGRADE_NODES operation
// that recreates the upgraded node on N: both nodes' kubelets report N
// again, the pool's version stays N, and the workload answered every
// second throughout.
// covers: google.container.v1.ClusterManager/RollbackNodePoolUpgrade, google.container.v1.ClusterManager/CancelOperation
func TestGKERollbackNodePoolUpgrade(t *testing.T) {
	h := New(t)
	g, _ := gkeClients(t, h)
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the probe must be loaded into the cluster")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	project, zone := h.Project(), "us-central1-b"
	parent := "projects/" + project + "/locations/" + zone
	older, newer, _ := gkeUpgradeVersions(t, g, parent)
	c := gkeOlderPoolCluster(ctx, t, g, project, zone, "ct-gke-rb", older, newer, 2, gkeNoAutoUpgrade())
	name := parent + "/clusters/ct-gke-rb"
	pool := name + "/nodePools/default-pool"
	kc := gkeKubeconfig(t, c)
	sel := "cloud.google.com/gke-nodepool=default-pool"
	if got := countVersions(gkeKubeletVersions(t, kc, sel)); got[kubeOf(older)] != 2 {
		t.Fatalf("nodes before the upgrade = %v, want 2 on %s", got, kubeOf(older))
	}
	_, err := g.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantPrivateCACode(t, "a rollback with no upgrade", err, codes.FailedPrecondition)

	gkeDeployProbe(ctx, t, instance, name, kc)
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
			if err := gkeProbeServes(kc); err != nil {
				if err := gkeProbeServes(kc); err != nil {
					failed.Add(1)
					lastErr.Store(err.Error())
				}
			}
		}
	}()
	defer func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		wg.Wait()
	}()

	op, err := g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newer, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatalf("UpdateNodePool: %v", err)
	}
	opName := parent + "/operations/" + op.GetName()
	_, err = g.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantPrivateCACode(t, "a rollback of a running surge upgrade", err, codes.FailedPrecondition)
	if err := g.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: opName}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if got, _ := g.GetOperation(ctx, &containerpb.GetOperationRequest{Name: opName}); got.GetStatus() != containerpb.Operation_ABORTING && got.GetStatus() != containerpb.Operation_DONE {
		t.Errorf("a cancelled upgrade's status = %s", got.GetStatus())
	}
	done := gkeWaitEnd(t, g, project, zone, op.GetName())
	if codes.Code(done.GetError().GetCode()) != codes.Canceled {
		t.Errorf("the cancelled upgrade ended with %v, want CANCELLED", done.GetError())
	}
	err = g.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: opName})
	wantPrivateCACode(t, "cancelling a done operation", err, codes.FailedPrecondition)
	if got := countVersions(gkeKubeletVersions(t, kc, sel)); got[kubeOf(newer)] != 1 || got[kubeOf(older)] != 1 {
		t.Fatalf("nodes after the cancel = %v, want one on each version", got)
	}
	np, err := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	if np.GetVersion() != older {
		t.Errorf("the partly upgraded pool's version = %s, want %s", np.GetVersion(), older)
	}

	rb, err := g.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool, RespectPdb: true})
	if err != nil {
		t.Fatalf("RollbackNodePoolUpgrade: %v", err)
	}
	if rb.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("the rollback's operation type = %s", rb.GetOperationType())
	}
	gkeWait(t, g, project, zone, rb)
	close(stop)
	wg.Wait()
	if failed.Load() > 0 {
		t.Errorf("the workload stopped serving %d of %d times (last: %v)", failed.Load(), asked.Load(), lastErr.Load())
	}
	if asked.Load() < 5 {
		t.Errorf("the probe was asked only %d times", asked.Load())
	}
	vs := gkeKubeletVersions(t, kc, sel)
	if got := countVersions(vs); len(vs) != 2 || got[kubeOf(older)] != 2 {
		t.Errorf("nodes after the rollback = %v, want 2 on %s", vs, kubeOf(older))
	}
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	np = c.GetNodePools()[0]
	if np.GetVersion() != older || np.GetStatus() != containerpb.NodePool_RUNNING || c.GetCurrentNodeVersion() != older {
		t.Errorf("after the rollback: pool %s %s, cluster nodes %s", np.GetVersion(), np.GetStatus(), c.GetCurrentNodeVersion())
	}
	gkeKubectl(t, kc, "", "-n", "upgrade", "rollout", "status", "deployment/probe", "--timeout=120s")
	if err := gkeProbeServes(kc); err != nil {
		t.Errorf("the probe does not serve after the rollback: %v", err)
	}
}

// TestGKEAutoUpgradePausesPastItsWindow (#2076), through the official Go
// client, as Google's maintenance-windows-and-exclusions page documents
// it: "If tasks run beyond the maintenance window, GKE attempts to pause
// the tasks" and "attempts to resume those tasks during the next
// maintenance window". A cluster's control plane runs N+1 and its
// two-node, auto-upgrading default-pool N, with a daily window (four
// hours) starting an hour from the Kubernetes Engine clock's now. Moving
// the clock (CloudBurrow's clock:advance) into the window starts the
// pool's automatic upgrade (UPGRADE_NODES); moving it past the window's
// end while the first node is upgraded pauses it: the operation is DONE,
// with no error and a message saying so, one node on each version, the
// pool's version still N. Outside the window nothing resumes; the next
// day's window does, and both nodes run N+1.
// covers: google.container.v1.ClusterManager/SetMaintenancePolicy
func TestGKEAutoUpgradePausesPastItsWindow(t *testing.T) {
	h := New(t)
	g, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	project, zone := h.Project(), "us-central1-a"
	parent := "projects/" + project + "/locations/" + zone
	older, newer, _ := gkeUpgradeVersions(t, g, parent)
	now := gkeClockAdvance(t, h, 0)
	start := now.UTC().Add(time.Hour).Truncate(time.Minute)
	policy := &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		Policy: &containerpb.MaintenanceWindow_DailyMaintenanceWindow{DailyMaintenanceWindow: &containerpb.DailyMaintenanceWindow{StartTime: start.Format("15:04")}}}}
	c := gkeOlderPoolCluster(ctx, t, g, project, zone, "ct-gke-win", older, newer, 2, policy)
	name := parent + "/clusters/ct-gke-win"
	pool := name + "/nodePools/default-pool"
	if d := c.GetMaintenancePolicy().GetWindow().GetDailyMaintenanceWindow().GetDuration(); d != "PT4H0M0S" {
		t.Errorf("daily window duration = %q", d)
	}
	kc := gkeKubeconfig(t, c)
	sel := "cloud.google.com/gke-nodepool=default-pool"

	poolOps := func() []*containerpb.Operation {
		ops, err := g.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		var out []*containerpb.Operation
		for _, o := range ops.GetOperations() {
			if strings.HasSuffix(o.GetTargetLink(), "/clusters/ct-gke-win/nodePools/default-pool") && o.GetOperationType() == containerpb.Operation_UPGRADE_NODES {
				out = append(out, o)
			}
		}
		return out
	}
	if ops := poolOps(); len(ops) != 0 {
		t.Fatalf("an upgrade ran outside the window: %v", ops)
	}
	// Into the window: the automatic upgrade starts; then past its end.
	gkeClockAdvance(t, h, start.Add(time.Minute).Sub(now))
	ops := poolOps()
	if len(ops) != 1 {
		t.Fatalf("automatic upgrades in the window = %v, want 1", ops)
	}
	gkeClockAdvance(t, h, 4*time.Hour)
	paused := gkeWaitEnd(t, g, project, zone, ops[0].GetName())
	if paused.GetError() != nil || !strings.Contains(paused.GetStatusMessage(), "paused") {
		t.Fatalf("the upgrade past its window ended %v", paused)
	}
	if got := countVersions(gkeKubeletVersions(t, kc, sel)); got[kubeOf(newer)] != 1 || got[kubeOf(older)] != 1 {
		t.Errorf("nodes when paused = %v, want one on each version", got)
	}
	np, err := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	if np.GetVersion() != older {
		t.Errorf("the paused pool's version = %s, want %s", np.GetVersion(), older)
	}
	// Outside the window, the minute's check starts nothing.
	time.Sleep(70 * time.Second)
	if ops := poolOps(); len(ops) != 1 {
		t.Fatalf("resumed outside the window: %v", ops)
	}
	// The next day's window resumes it.
	gkeClockAdvance(t, h, 20*time.Hour)
	ops = poolOps()
	if len(ops) != 2 {
		t.Fatalf("operations after the next window opened = %v, want the resumed one", ops)
	}
	for _, o := range ops {
		if o.GetStatus() != containerpb.Operation_DONE {
			gkeWait(t, g, project, zone, o)
		}
	}
	np, _ = g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.GetVersion() != newer || np.GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("after the resumed upgrade: %s %s", np.GetVersion(), np.GetStatus())
	}
	vs := gkeKubeletVersions(t, kc, sel)
	if got := countVersions(vs); len(vs) != 2 || got[kubeOf(newer)] != 2 {
		t.Errorf("nodes after the resumed upgrade = %v, want 2 on %s", vs, kubeOf(newer))
	}
}

// TestGcloudContainerNodePoolsRollback (#2076): `gcloud container
// node-pools rollback NAME --cluster --respect-pdb`, as its reference page
// gives it, after the official Go client cancels an upgrade of a two-node
// pool that ran N, with one node upgraded: gcloud waits for the rollback,
// and both nodes run N again; a second rollback, with nothing left to roll
// back, fails.
func TestGcloudContainerNodePoolsRollback(t *testing.T) {
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
	const zone, cluster = "us-central1-f", "gc-gke-rb"
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
	c := gkeOlderPoolCluster(ctx, t, g, project, zone, cluster, older, newer, 2, gkeNoAutoUpgrade())
	pool := parent + "/clusters/" + cluster + "/nodePools/default-pool"
	op, err := g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newer, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatalf("UpdateNodePool: %v", err)
	}
	if err := g.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: parent + "/operations/" + op.GetName()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	gkeWaitEnd(t, g, project, zone, op.GetName())
	kc := gkeKubeconfig(t, c)
	sel := "cloud.google.com/gke-nodepool=default-pool"
	if got := countVersions(gkeKubeletVersions(t, kc, sel)); got[kubeOf(newer)] != 1 {
		t.Fatalf("nodes after the cancel = %v, want one on %s", got, kubeOf(newer))
	}
	if out, err := gc("container", "node-pools", "rollback", "default-pool", "--cluster", cluster, "--zone", zone, "--respect-pdb=true"); err != nil {
		t.Fatalf("gcloud container node-pools rollback: %v\n%s", err, out)
	}
	vs := gkeKubeletVersions(t, kc, sel)
	if got := countVersions(vs); len(vs) != 2 || got[kubeOf(older)] != 2 {
		t.Errorf("nodes after gcloud's rollback = %v, want 2 on %s", vs, kubeOf(older))
	}
	if out, err := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(version,status)"); err != nil ||
		strings.Join(strings.Fields(out), " ") != older+" RUNNING" {
		t.Errorf("node-pools describe after the rollback = %q (%v)", out, err)
	}
	if out, err := gc("container", "operations", "list", "--zone", zone, "--format=value(operationType,status)"); err != nil || !strings.Contains(out, "UPGRADE_NODES") {
		t.Errorf("operations list:\n%s", out)
	}
	if out, err := gc("container", "node-pools", "rollback", "default-pool", "--cluster", cluster, "--zone", zone); err == nil {
		t.Errorf("a second rollback succeeded:\n%s", out)
	}
}
