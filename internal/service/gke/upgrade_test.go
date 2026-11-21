package gke

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	newV, newImg = "1.36.4-gke.0", "kindest/node:v1.36.4"
	oldV, oldImg = "1.35.8-gke.0", "kindest/node:v1.35.8"
)

// upFake is a Backend and an Upgrader: its pools' nodes carry images, and
// every node action is logged in order.
type upFake struct {
	*fakeBackend
	umu    sync.Mutex
	pools  map[string][]Node // pool -> nodes
	next   int
	log    []string
	master string
	// hook, when set, runs before each UpgradeControlPlane, AddNode and
	// RemoveNode, outside the fake's lock (#2076's tests stop an upgrade
	// midway with it; #2069's hold one running).
	hook func(verb, name string)
}

func newUpFake() *upFake { return &upFake{fakeBackend: newFake(), pools: map[string][]Node{}} }

func (f *upFake) Resize(ctx context.Context, kind, image, pool string, n int) error {
	f.umu.Lock()
	defer f.umu.Unlock()
	for len(f.pools[pool]) < n {
		f.next++
		f.pools[pool] = append(f.pools[pool], Node{Pool: pool, Name: fmt.Sprintf("n%d", f.next), Image: image})
	}
	f.pools[pool] = f.pools[pool][:n]
	return nil
}

func (f *upFake) UpgradeControlPlane(_ context.Context, _, image string) error {
	f.runHook("master", image)
	f.umu.Lock()
	defer f.umu.Unlock()
	f.master = image
	f.log = append(f.log, "master "+image)
	return nil
}

func (f *upFake) PoolNodes(_ context.Context, _, pool string) ([]Node, error) {
	f.umu.Lock()
	defer f.umu.Unlock()
	return append([]Node(nil), f.pools[pool]...), nil
}

func (f *upFake) runHook(verb, name string) {
	f.umu.Lock()
	h := f.hook
	f.umu.Unlock()
	if h != nil {
		h(verb, name)
	}
}

func (f *upFake) setHook(h func(verb, name string)) {
	f.umu.Lock()
	f.hook = h
	f.umu.Unlock()
}

func (f *upFake) AddNode(_ context.Context, _, image, pool string) (string, error) {
	f.runHook("add", image)
	f.umu.Lock()
	defer f.umu.Unlock()
	f.next++
	n := fmt.Sprintf("n%d", f.next)
	f.pools[pool] = append(f.pools[pool], Node{Pool: pool, Name: n, Image: image})
	f.log = append(f.log, "add "+n)
	return n, nil
}

func (f *upFake) act(verb, name string) error {
	f.umu.Lock()
	defer f.umu.Unlock()
	f.log = append(f.log, verb+" "+name)
	return nil
}

func (f *upFake) CordonNode(_ context.Context, _, name string) error { return f.act("cordon", name) }
func (f *upFake) UncordonNode(_ context.Context, _, name string) error {
	return f.act("uncordon", name)
}
func (f *upFake) DrainNode(_ context.Context, _, name string) error { return f.act("drain", name) }
func (f *upFake) ForceDrainNode(_ context.Context, _, name string) error {
	return f.act("force-drain", name)
}
func (f *upFake) RemoveNode(_ context.Context, _, name string) error {
	f.runHook("remove", name)
	f.umu.Lock()
	defer f.umu.Unlock()
	f.log = append(f.log, "remove "+name)
	for p, ns := range f.pools {
		for i, n := range ns {
			if n.Name == name {
				f.pools[p] = append(ns[:i:i], ns[i+1:]...)
			}
		}
	}
	return nil
}

func (f *upFake) events() string {
	f.umu.Lock()
	defer f.umu.Unlock()
	return strings.Join(f.log, ", ")
}

func newUpServer(t *testing.T, now func() time.Time) (*Server, *upFake) {
	t.Helper()
	f := newUpFake()
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", Now: now, AutoUpgradeInterval: -1,
		Versions: []Version{{GKE: newV, Image: newImg}, {GKE: oldV, Image: oldImg}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, f
}

func makeOld(t *testing.T, s *Server, name string, n int32, mp *containerpb.MaintenancePolicy, us *containerpb.NodePool_UpgradeSettings) string {
	t.Helper()
	ctx := context.Background()
	parent := "projects/p/locations/us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: name, InitialClusterVersion: "1.35", MaintenancePolicy: mp,
		NodePools: []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: n, UpgradeSettings: us}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	return parent + "/clusters/" + name
}

func TestUpgradeControlPlaneThenSurgeNodes(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	name := makeOld(t, s, "c1", 2, nil, nil)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.CurrentMasterVersion != oldV || c.NodePools[0].Version != oldV {
		t.Fatalf("made on %s / %s", c.CurrentMasterVersion, c.NodePools[0].Version)
	}
	us := c.NodePools[0].GetUpgradeSettings()
	if us.GetMaxSurge() != 1 || us.GetStrategy() != containerpb.NodePoolUpdateStrategy_SURGE {
		t.Errorf("default upgrade settings = %v", us)
	}
	// Nodes first is refused: node pools are never newer than the control
	// plane.
	_, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	wantCode(t, "nodes before the control plane", err, codes.InvalidArgument)

	op, err := s.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name, MasterVersion: "1.36"})
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != containerpb.Operation_UPGRADE_MASTER || op.Status != containerpb.Operation_RUNNING {
		t.Errorf("master op = %v", op)
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.CurrentMasterVersion != newV || c.CurrentNodeVersion != oldV || c.Status != containerpb.Cluster_RUNNING {
		t.Errorf("after the master upgrade: master %s, nodes %s, %s", c.CurrentMasterVersion, c.CurrentNodeVersion, c.Status)
	}

	op, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: "-", ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("node op type = %s", op.OperationType)
	}
	got := wait(t, s, "p", "us-central1-a", op)
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	// Surge 1, unavailable 0: one new node Ready before each old node goes.
	want := "master " + newImg + ", add n3, cordon n1, drain n1, remove n1, add n4, cordon n2, drain n2, remove n2"
	if e := f.events(); e != want {
		t.Errorf("events = %s\nwant      %s", e, want)
	}
	if m := got.GetProgress().GetMetrics(); len(m) != 2 || m[1].GetIntValue() != 2 {
		t.Errorf("progress = %v", got.GetProgress())
	}
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.CurrentNodeVersion != newV || c.NodePools[0].Version != newV || c.NodePools[0].Status != containerpb.NodePool_RUNNING {
		t.Errorf("after the node upgrade: %s, %v", c.CurrentNodeVersion, c.NodePools[0])
	}
	for _, n := range f.pools["default-pool"] {
		if n.Image != newImg {
			t.Errorf("node %s runs %s", n.Name, n.Image)
		}
	}
}

func TestUpgradeMaxUnavailableAndRefusals(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	name := makeOld(t, s, "c2", 2, nil, &containerpb.NodePool_UpgradeSettings{MaxSurge: 0, MaxUnavailable: 2})
	_, err := s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.2.3"}})
	wantCode(t, "an unserved version", err, codes.InvalidArgument)
	_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/default-pool", NodeVersion: "-", ImageType: "COS_CONTAINERD",
		UpgradeSettings: &containerpb.NodePool_UpgradeSettings{}})
	wantCode(t, "surge and unavailable both 0", err, codes.InvalidArgument)
	// Hold the control plane upgrade until the second request is answered:
	// the fake upgrades at once, so without the hold the operation can be
	// DONE (and "latest" already current) before the second request runs.
	reached, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(verb, _ string) {
		if verb == "master" {
			close(reached)
			<-release
		}
	})
	op, err := s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "latest"}})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "latest"}})
	wantCode(t, "a second operation", err, codes.FailedPrecondition)
	close(release)
	f.setHook(nil)
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	// Downgrading the control plane is refused.
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: oldV}})
	wantCode(t, "a downgrade", err, codes.InvalidArgument)
	// UpdateCluster's desired_node_version, the pool implied as the only one.
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredNodeVersion: "-"}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1-a", op)
	want := "master " + newImg + ", cordon n1, cordon n2, drain n1, remove n1, drain n2, remove n2, add n3, add n4"
	if e := f.events(); e != want {
		t.Errorf("events = %s\nwant      %s", e, want)
	}
}

func TestUpgradeBlueGreenWithCompleteNodePoolUpgrade(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	bg := containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	name := makeOld(t, s, "c3", 2, nil, &containerpb.NodePool_UpgradeSettings{Strategy: &bg,
		BlueGreenSettings: &containerpb.BlueGreenSettings{
			RolloutPolicy: &containerpb.BlueGreenSettings_StandardRolloutPolicy_{StandardRolloutPolicy: &containerpb.BlueGreenSettings_StandardRolloutPolicy{
				UpdateBatchSize: &containerpb.BlueGreenSettings_StandardRolloutPolicy_BatchNodeCount{BatchNodeCount: 1}}},
			NodePoolSoakDuration: durationpb.New(time.Hour)}})
	op, _ := s.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name, MasterVersion: newV})
	wait(t, s, "p", "us-central1-a", op)
	pool := name + "/nodePools/default-pool"
	_, err := s.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool})
	wantCode(t, "complete with no upgrade", err, codes.FailedPrecondition)
	op, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
		if np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() == containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING {
			if np.GetUpdateInfo().GetBlueGreenInfo().GetGreenPoolVersion() != newV {
				t.Errorf("green pool version = %v", np.GetUpdateInfo())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never soaking: %v", np.GetUpdateInfo())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool}); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	want := "master " + newImg + ", add n3, add n4, cordon n1, cordon n2, drain n1, drain n2, remove n1, remove n2"
	if e := f.events(); e != want {
		t.Errorf("events = %s\nwant      %s", e, want)
	}
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.Version != newV || np.UpdateInfo != nil {
		t.Errorf("after blue-green: %v", np)
	}
}

func TestAutoUpgradeRespectsWindowsAndExclusions(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) // a Friday
	clock := func() time.Time { return now }
	s, f := newUpServer(t, clock)
	ctx := context.Background()
	excl := &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		Policy: &containerpb.MaintenanceWindow_DailyMaintenanceWindow{DailyMaintenanceWindow: &containerpb.DailyMaintenanceWindow{StartTime: "03:00"}},
		MaintenanceExclusions: map[string]*containerpb.TimeWindow{"freeze": {
			StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now.Add(48 * time.Hour)),
			Options: &containerpb.TimeWindow_MaintenanceExclusionOptions{MaintenanceExclusionOptions: &containerpb.MaintenanceExclusionOptions{
				Scope: containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES}}}}}}
	name := makeOld(t, s, "c4", 1, excl, nil)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if d := c.GetMaintenancePolicy().GetWindow().GetDailyMaintenanceWindow().GetDuration(); d != "PT4H0M0S" || c.GetMaintenancePolicy().GetResourceVersion() == "" {
		t.Errorf("maintenance policy = %v", c.GetMaintenancePolicy())
	}
	// Outside the 03:00 window, and inside a "no minor upgrades" exclusion.
	if ops := s.AutoUpgrade(); len(ops) != 0 {
		t.Fatalf("auto-upgrade started outside its window: %v", ops)
	}
	// In the window (03:00–07:00 tomorrow) but still excluded.
	now = time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
	if ops := s.AutoUpgrade(); len(ops) != 0 {
		t.Fatalf("auto-upgrade started in an exclusion: %v", ops)
	}
	// Past the exclusion, in a window: the control plane first.
	now = time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC)
	ops := s.AutoUpgrade()
	if len(ops) != 1 || ops[0].OperationType != containerpb.Operation_UPGRADE_MASTER {
		t.Fatalf("auto-upgrade = %v", ops)
	}
	wait(t, s, "p", "us-central1-a", ops[0])
	// Then the node pool, auto-upgrade being on by default.
	ops = s.AutoUpgrade()
	if len(ops) != 1 || ops[0].OperationType != containerpb.Operation_UPGRADE_NODES {
		t.Fatalf("node auto-upgrade = %v", ops)
	}
	wait(t, s, "p", "us-central1-a", ops[0])
	if ops := s.AutoUpgrade(); len(ops) != 0 {
		t.Errorf("nothing left to upgrade, yet %v", ops)
	}
	if e := f.events(); !strings.HasPrefix(e, "master "+newImg+", add ") {
		t.Errorf("events = %s", e)
	}

	// A manual upgrade ignores an exclusion (documented).
	name2 := makeOld(t, s, "c5", 1, &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		MaintenanceExclusions: map[string]*containerpb.TimeWindow{"all": {StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now.Add(time.Hour))}}}}, nil)
	if ops := s.AutoUpgrade(); len(ops) != 0 {
		t.Errorf("a NO_UPGRADES exclusion let %v start", ops)
	}
	op, err := s.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name2, MasterVersion: newV})
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
}

func TestMaintenancePolicyValidationAndWindows(t *testing.T) {
	s, _ := newUpServer(t, nil)
	ctx := context.Background()
	name := makeOld(t, s, "c6", 1, nil, nil)
	start := time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC) // a Monday
	weekly := &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{Policy: &containerpb.MaintenanceWindow_RecurringWindow{
		RecurringWindow: &containerpb.RecurringTimeWindow{Window: &containerpb.TimeWindow{StartTime: timestamppb.New(start), EndTime: timestamppb.New(start.Add(6 * time.Hour))},
			Recurrence: "FREQ=WEEKLY;BYDAY=SA,SU"}}}}
	if _, err := s.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name, MaintenancePolicy: weekly}); err != nil {
		t.Fatal(err)
	}
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC): true,  // Saturday 23:00
		time.Date(2026, 11, 22, 3, 30, 0, 0, time.UTC): true,  // Saturday's window, past midnight
		time.Date(2026, 11, 22, 4, 30, 0, 0, time.UTC): false, // after it
		time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC): false, // Tuesday
	} {
		if got := AutoUpgradeAllowed(weekly, at, false, true); got != want {
			t.Errorf("weekly window at %s = %v, want %v", at, got, want)
		}
	}
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	_, err := s.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name,
		MaintenancePolicy: &containerpb.MaintenancePolicy{ResourceVersion: "stale"}})
	wantCode(t, "a stale resource version", err, codes.FailedPrecondition)
	ex := map[string]*containerpb.TimeWindow{}
	for i := 0; i < 4; i++ {
		ex[fmt.Sprint("x", i)] = &containerpb.TimeWindow{StartTime: timestamppb.New(start), EndTime: timestamppb.New(start.Add(time.Hour))}
	}
	_, err = s.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name,
		MaintenancePolicy: &containerpb.MaintenancePolicy{ResourceVersion: c.GetMaintenancePolicy().GetResourceVersion(), Window: &containerpb.MaintenanceWindow{MaintenanceExclusions: ex}}})
	wantCode(t, "four no-upgrades exclusions", err, codes.InvalidArgument)
	_, err = s.SetMaintenancePolicy(ctx, &containerpb.SetMaintenancePolicyRequest{Name: name, MaintenancePolicy: &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		Policy: &containerpb.MaintenanceWindow_DailyMaintenanceWindow{DailyMaintenanceWindow: &containerpb.DailyMaintenanceWindow{StartTime: "25:00"}}}}})
	wantCode(t, "a bad daily start", err, codes.InvalidArgument)
}
