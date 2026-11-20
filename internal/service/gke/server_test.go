package gke

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeBackend struct {
	mu      sync.Mutex
	free    uint64
	nodes   map[string]map[string]int // kind -> pool -> n
	created map[string]bool
}

func newFake() *fakeBackend {
	return &fakeBackend{free: 100 << 30, nodes: map[string]map[string]int{}, created: map[string]bool{}}
}

func (f *fakeBackend) Create(_ context.Context, kind, _ string, _ bool) (Access, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[kind] = true
	f.nodes[kind] = map[string]int{}
	return Access{Endpoint: "127.0.0.1:6443", CACert: "Q0E=", ClientCert: "Y2VydA==", ClientKey: "a2V5"}, nil
}

func (f *fakeBackend) Resize(_ context.Context, kind, _, pool string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[kind][pool] = n
	return nil
}

func (f *fakeBackend) Delete(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.created, kind)
	delete(f.nodes, kind)
	return nil
}

func (f *fakeBackend) FreeBytes() (uint64, error) { return f.free, nil }

func newTestServer(t *testing.T, state string) (*Server, *fakeBackend) {
	t.Helper()
	f := newFake()
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", State: state, Versions: []Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, f
}

func wait(t *testing.T, s *Server, project, location string, op *containerpb.Operation) *containerpb.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: opName(project, location, op.Name)})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == containerpb.Operation_DONE {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s did not finish", op.Name)
	return nil
}

func wantCode(t *testing.T, what string, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Errorf("%s: %v, want %s", what, err, c)
	}
}

func TestClusterLifecycle(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "gke.json")
	s, f := newTestServer(t, state)
	parent := "projects/p1/locations/us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "c1", InitialNodeCount: 2,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != containerpb.Operation_CREATE_CLUSTER || op.Status != containerpb.Operation_RUNNING {
		t.Errorf("create op = %v", op)
	}
	if done := wait(t, s, "p1", "us-central1-a", op); done.Error != nil {
		t.Fatalf("create failed: %v", done.Error)
	}
	c, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != containerpb.Cluster_RUNNING || c.Endpoint != "127.0.0.1:6443" || c.MasterAuth.ClientCertificate == "" ||
		len(c.NodePools) != 1 || c.NodePools[0].Name != "default-pool" || c.CurrentNodeCount != 2 {
		t.Errorf("cluster = %v", c)
	}
	kind := KindName("cloudburrow-t", parent+"/clusters/c1")
	if f.nodes[kind]["default-pool"] != 2 {
		t.Errorf("backend nodes = %v", f.nodes)
	}
	// Duplicates and bad names.
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	wantCode(t, "duplicate", err, codes.AlreadyExists)
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "Bad_Name", InitialNodeCount: 1}})
	wantCode(t, "bad name", err, codes.InvalidArgument)
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "zero"}})
	wantCode(t, "no nodes", err, codes.InvalidArgument)
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ap", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	wantCode(t, "zonal autopilot", err, codes.InvalidArgument)
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "big", InitialNodeCount: 10}})
	wantCode(t, "too many nodes", err, codes.ResourceExhausted)
	_, err = s.GetCluster(ctx, &containerpb.GetClusterRequest{ProjectId: "p1", Zone: "us-central1-a", ClusterId: "nope"})
	wantCode(t, "missing", err, codes.NotFound)

	// Node pools.
	op, err = s.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: parent + "/clusters/c1", NodePool: &containerpb.NodePool{Name: "pool-2", InitialNodeCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	op, err = s.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: parent + "/clusters/c1/nodePools/pool-2", NodeCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	np, err := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: parent + "/clusters/c1/nodePools/pool-2"})
	if err != nil || np.InitialNodeCount != 3 || f.nodes[kind]["pool-2"] != 3 {
		t.Errorf("resized pool = %v, %v; nodes %v", np, err, f.nodes[kind])
	}
	op, err = s.DeleteNodePool(ctx, &containerpb.DeleteNodePoolRequest{Name: parent + "/clusters/c1/nodePools/pool-2"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	if l, _ := s.ListNodePools(ctx, &containerpb.ListNodePoolsRequest{Parent: parent + "/clusters/c1"}); len(l.NodePools) != 1 {
		t.Errorf("pools after delete = %v", l)
	}

	// Labels with their fingerprint.
	_, err = s.SetLabels(ctx, &containerpb.SetLabelsRequest{Name: parent + "/clusters/c1", ResourceLabels: map[string]string{"env": "dev"}, LabelFingerprint: "wrong"})
	wantCode(t, "stale fingerprint", err, codes.FailedPrecondition)
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	op, err = s.SetLabels(ctx, &containerpb.SetLabelsRequest{Name: parent + "/clusters/c1", ResourceLabels: map[string]string{"env": "dev"}, LabelFingerprint: c.LabelFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)

	// Update: a field not served is UNIMPLEMENTED; an unknown version is refused.
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: parent + "/clusters/c1", Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.20"}})
	wantCode(t, "unknown version", err, codes.InvalidArgument)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: parent + "/clusters/c1", Update: &containerpb.ClusterUpdate{DesiredLocations: []string{"x"}}})
	wantCode(t, "unserved field", err, codes.Unimplemented)

	// The records survive a restart.
	s2, _ := newTestServer(t, state)
	l, err := s2.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/p1/locations/-"})
	if err != nil || len(l.Clusters) != 1 || l.Clusters[0].ResourceLabels["env"] != "dev" {
		t.Errorf("restored clusters = %v, %v", l, err)
	}

	op, err = s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: parent + "/clusters/c1"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	if f.created[kind] {
		t.Error("the backing cluster was not deleted")
	}
	ops, _ := s.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: "projects/p1/locations/-"})
	if len(ops.Operations) < 6 {
		t.Errorf("operations = %d", len(ops.Operations))
	}
}

func TestDiskThresholdRefusesCreate(t *testing.T) {
	s, f := newTestServer(t, "")
	f.free = 1 << 30
	_, err := s.CreateCluster(context.Background(), &containerpb.CreateClusterRequest{Parent: "projects/p/locations/us-central1-a",
		Cluster: &containerpb.Cluster{Name: "c", InitialNodeCount: 1}})
	wantCode(t, "low disk", err, codes.ResourceExhausted)
}

func TestAutopilotHasNoPoolsToManage(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, "")
	parent := "projects/p/locations/us-central1"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "ap", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1", op)
	_, err = s.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: parent + "/clusters/ap", NodePool: &containerpb.NodePool{Name: "x", InitialNodeCount: 1}})
	wantCode(t, "autopilot pool", err, codes.FailedPrecondition)
	cfg, err := s.GetServerConfig(ctx, &containerpb.GetServerConfigRequest{Name: parent})
	if err != nil || cfg.DefaultClusterVersion != "1.36.4-gke.0" || len(cfg.Channels) != 4 {
		t.Errorf("server config = %v, %v", cfg, err)
	}
}
