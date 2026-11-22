package gke

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// configFake is a rotatingFake that also keeps each pool's node labels
// and taints.
type configFake struct {
	*rotatingFake
	cmu    sync.Mutex
	labels map[string]map[string]string
	taints map[string][]NodeTaint
}

func (f *configFake) ConfigureNodes(_ context.Context, _, pool string, labels map[string]string, taints []NodeTaint, _ bool) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.labels[pool], f.taints[pool] = labels, taints
	return nil
}

func newConfigServer(t *testing.T) (*Server, *configFake) {
	t.Helper()
	f := &configFake{rotatingFake: &rotatingFake{fakeBackend: newFake(), zones: map[string][]string{}},
		labels: map[string]map[string]string{}, taints: map[string][]NodeTaint{}}
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", State: filepath.Join(t.TempDir(), "gke.json"),
		Versions: []Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}, AutoUpgradeInterval: -1, AutoscaleInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, f
}

// TestNodeLabelsTaintsAndLocations (#2100): a cluster made with locations
// places its pools' nodes in them, and node_config labels and taints reach
// the backend when the pool is made; malformed ones, a taint with no
// effect and zones outside the region are INVALID_ARGUMENT. A pool's own
// locations replace the cluster's. UpdateNodePool's labels and taints
// replace the pool's, and its locations move its nodes (the node count
// per zone kept), each an UPGRADE_NODES operation. Every pool's nodes carry
// its machine type (node.kubernetes.io/instance-type). A malformed machine
// type, a disk under 10 GB, and a machine type with labels at once are
// INVALID_ARGUMENT; kubelet_config is still UNIMPLEMENTED.
func TestNodeLabelsTaintsAndLocations(t *testing.T) {
	s, f := newConfigServer(t)
	ctx := context.Background()
	parent := "projects/p1/locations/us-central1-a"
	name := parent + "/clusters/c1"
	mk := func(c *containerpb.Cluster) error {
		_, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: c})
		return err
	}
	bad := []*containerpb.Cluster{
		{Name: "b1", Locations: []string{"us-central1-a", "us-east1-b"}, NodePools: []*containerpb.NodePool{{Name: "p", InitialNodeCount: 1}}},
		{Name: "b2", NodePools: []*containerpb.NodePool{{Name: "p", InitialNodeCount: 1, Config: &containerpb.NodeConfig{Labels: map[string]string{"-bad": "x"}}}}},
		{Name: "b3", NodePools: []*containerpb.NodePool{{Name: "p", InitialNodeCount: 1, Config: &containerpb.NodeConfig{
			Taints: []*containerpb.NodeTaint{{Key: "dedicated", Value: "gpu"}}}}}},
		{Name: "b4", NodePools: []*containerpb.NodePool{{Name: "p", InitialNodeCount: 1, Locations: []string{"europe-west1-b"}}}},
	}
	for _, c := range bad {
		wantCode(t, "cluster "+c.Name, mk(c), codes.InvalidArgument)
	}
	taint := []*containerpb.NodeTaint{{Key: "dedicated", Value: "gpu", Effect: containerpb.NodeTaint_NO_SCHEDULE}}
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1",
		Locations: []string{"us-central1-b", "us-central1-a"},
		NodePools: []*containerpb.NodePool{
			{Name: "pool", InitialNodeCount: 1, Config: &containerpb.NodeConfig{Labels: map[string]string{"team": "a", "example.com/tier": "web"}, Taints: taint}},
			{Name: "own", InitialNodeCount: 1, Locations: []string{"us-central1-f"}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if got := strings.Join(c.GetLocations(), ","); got != "us-central1-a,us-central1-b" {
		t.Errorf("cluster locations = %s", got)
	}
	if got := f.zones["pool"]; !reflect.DeepEqual(got, []string{"us-central1-a", "us-central1-b"}) {
		t.Errorf("pool placed in %v", got)
	}
	if got := f.zones["own"]; !reflect.DeepEqual(got, []string{"us-central1-f"}) {
		t.Errorf("own placed in %v", got)
	}
	if f.labels["pool"]["team"] != "a" || f.labels["pool"]["example.com/tier"] != "web" || f.labels["pool"][instanceTypeLabel] != "e2-medium" ||
		!reflect.DeepEqual(f.taints["pool"], []NodeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}}) {
		t.Errorf("pool configured with %v %v", f.labels["pool"], f.taints["pool"])
	}
	if !reflect.DeepEqual(f.labels["own"], map[string]string{instanceTypeLabel: "e2-medium", bootDiskLabel: "pd-balanced"}) {
		t.Errorf("a pool with no labels of its own was configured with %v, want its machine type's", f.labels["own"])
	}

	pool := name + "/nodePools/pool"
	op, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool,
		Labels: &containerpb.NodeLabels{Labels: map[string]string{"team": "b"}},
		Taints: &containerpb.NodeTaints{Taints: []*containerpb.NodeTaint{{Key: "spot", Effect: containerpb.NodeTaint_PREFER_NO_SCHEDULE}}}})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("labels update operation type = %s", op.GetOperationType())
	}
	wait(t, s, "p1", "us-central1-a", op)
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if !reflect.DeepEqual(np.GetConfig().GetLabels(), map[string]string{"team": "b"}) || len(np.GetConfig().GetTaints()) != 1 ||
		np.GetConfig().GetTaints()[0].GetKey() != "spot" {
		t.Errorf("after the update: labels %v taints %v", np.GetConfig().GetLabels(), np.GetConfig().GetTaints())
	}
	if !reflect.DeepEqual(f.labels["pool"], map[string]string{"team": "b", instanceTypeLabel: "e2-medium", bootDiskLabel: "pd-balanced"}) || f.taints["pool"][0].Effect != "PreferNoSchedule" {
		t.Errorf("backend after the update: %v %v", f.labels["pool"], f.taints["pool"])
	}

	op, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, Locations: []string{"us-central1-c", "us-central1-a", "us-central1-b"}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	np, _ = s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if got := strings.Join(np.GetLocations(), ","); got != "us-central1-a,us-central1-b,us-central1-c" || len(np.GetInstanceGroupUrls()) != 3 {
		t.Errorf("pool locations = %s, instance groups %v", got, np.GetInstanceGroupUrls())
	}
	if got := f.fakeBackend.nodes[clusterKind(t, s, name)]["pool"]; got != 3 {
		t.Errorf("pool nodes after three zones = %d, want 3", got)
	}
	if !reflect.DeepEqual(f.labels["pool"], map[string]string{"team": "b", instanceTypeLabel: "e2-medium", bootDiskLabel: "pd-balanced"}) {
		t.Errorf("a locations update changed the labels: %v", f.labels["pool"])
	}

	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, Locations: []string{"us-west1-a"}})
	wantCode(t, "a zone of another region", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool,
		Taints: &containerpb.NodeTaints{Taints: []*containerpb.NodeTaint{{Key: "x"}}}})
	wantCode(t, "a taint with no effect", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, MachineType: "E2 standard"})
	wantCode(t, "a malformed machine type", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, DiskSizeGb: 5})
	wantCode(t, "a 5 GB disk", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, MachineType: "e2-standard-4",
		Labels: &containerpb.NodeLabels{Labels: map[string]string{"x": "y"}}})
	wantCode(t, "a machine type with labels", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, KubeletConfig: &containerpb.NodeKubeletConfig{CpuManagerPolicy: "static"}})
	wantCode(t, "kubelet_config", err, codes.Unimplemented)
}

// clusterKind is the backend's name for a cluster.
func clusterKind(t *testing.T, s *Server, name string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.clusters[name]
	if e == nil {
		t.Fatalf("no cluster %s", name)
	}
	return e.kind
}
