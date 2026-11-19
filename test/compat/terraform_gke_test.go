//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// TestTerraformContainerClusterAndNodePool (#1913): Google's recommended
// layout, a google_container_cluster with remove_default_node_pool and a
// separately managed google_container_node_pool, applies through
// `cloudburrow terraform` (container_custom_endpoint) with egress to Google
// blocked. The cluster is a real kind cluster: kubectl, through the
// endpoint and client certificate the official client reads back, sees the
// pool's node and not the default pool's. The plan is clean; new labels
// and a node_count of 2 apply in place (a second real node joins); the
// plan is clean again; destroy removes both.
// covers: google.container.v1.ClusterManager/CreateCluster, google.container.v1.ClusterManager/GetCluster, google.container.v1.ClusterManager/DeleteCluster, google.container.v1.ClusterManager/GetOperation, google.container.v1.ClusterManager/GetServerConfig, google.container.v1.ClusterManager/SetLabels, google.container.v1.ClusterManager/CreateNodePool, google.container.v1.ClusterManager/GetNodePool, google.container.v1.ClusterManager/DeleteNodePool, google.container.v1.ClusterManager/SetNodePoolSize
func TestTerraformContainerClusterAndNodePool(t *testing.T) {
	h := New(t)
	c, _ := gkeClients(t, h) // skips without CLOUDBURROW_TEST_GKE
	// Not h.Context()'s minute: the reads come after applies that make
	// kind clusters and nodes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	m := newTFModule(t)
	name := fmt.Sprintf("tfgke-%d", time.Now().UnixNano()%1e6)
	const zone = "us-central1-a"
	clusterName := "projects/" + m.project + "/locations/" + zone + "/clusters/" + name
	t.Cleanup(func() {
		// Whatever destroy left, by name: the kind cluster goes with it.
		if _, err := c.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName}); err == nil {
			if op, err := c.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: clusterName}); err == nil {
				gkeWait(t, c, m.project, zone, op)
			}
		}
	})
	module := func(env string, nodes int) string {
		return fmt.Sprintf(`
resource "google_container_cluster" "c" {
  project                  = %[1]q
  name                     = %[2]q
  location                 = %[3]q
  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false
  resource_labels = {
    env = %[4]q
  }
  master_auth {
    client_certificate_config {
      issue_client_certificate = true
    }
  }
}
resource "google_container_node_pool" "p" {
  project    = %[1]q
  name       = "tfpool"
  location   = %[3]q
  cluster    = google_container_cluster.c.name
  node_count = %[5]d
  node_config {
    machine_type = "e2-medium"
  }
}
`, m.project, name, zone, env, nodes)
	}
	m.write(module("dev", 1))
	m.must("init", "-input=false", "-no-color")
	m.apply()

	got, err := c.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil {
		t.Fatalf("the cluster Terraform made: %v", err)
	}
	if got.GetStatus() != containerpb.Cluster_RUNNING || got.GetResourceLabels()["env"] != "dev" {
		t.Fatalf("the cluster Terraform made is %v with labels %v", got.GetStatus(), got.GetResourceLabels())
	}
	if len(got.GetNodePools()) != 1 || got.GetNodePools()[0].GetName() != "tfpool" {
		t.Fatalf("the cluster's node pools are %v, want tfpool alone (the default pool removed)", got.GetNodePools())
	}
	kc := gkeKubeconfig(t, got)
	if n := gkeNodes(t, kc, "tfpool"); len(n) != 1 {
		t.Fatalf("kubectl sees tfpool's nodes %v, want 1", n)
	}
	if n := gkeNodes(t, kc, "default-pool"); len(n) != 0 {
		t.Errorf("kubectl still sees default-pool's nodes %v", n)
	}
	gkePlanClean(t, m)

	m.write(module("prod", 2))
	m.apply()
	got, err = c.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil || got.GetResourceLabels()["env"] != "prod" {
		t.Fatalf("after the update the cluster is %v, %v; want env=prod", got.GetResourceLabels(), err)
	}
	np, err := c.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: clusterName + "/nodePools/tfpool"})
	if err != nil || np.GetInitialNodeCount() != 2 {
		t.Fatalf("after the update tfpool is %v, %v; want 2 nodes", np, err)
	}
	if n := gkeNodes(t, kc, "tfpool"); len(n) != 2 {
		t.Errorf("kubectl sees tfpool's nodes %v after node_count = 2", n)
	}
	gkePlanClean(t, m)

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName}); err == nil {
		t.Errorf("the cluster survived destroy")
	}
}

// gkePlanClean is planClean with the whole plan on failure: a cluster's
// plan names the attribute that forces a replacement near its top, far
// above the last lines.
func gkePlanClean(t *testing.T, m *tfModule) {
	t.Helper()
	out, err := m.tf("plan", "-detailed-exitcode", "-input=false", "-no-color")
	if err != nil {
		t.Fatalf("plan after apply is not clean (%v):\n%s", err, out)
	}
}
