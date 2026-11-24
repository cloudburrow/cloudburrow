package gke

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// rotatingFake is a fakeBackend that also rotates, places and emulates.
type rotatingFake struct {
	*fakeBackend
	mu       sync.Mutex
	ca       int
	zones    map[string][]string
	emulated string
}

func (f *rotatingFake) access() Access {
	return Access{Endpoint: "127.0.0.1:6443", CACert: string(rune('A' + f.ca)), ClientCert: "Y2VydA==", ClientKey: "a2V5"}
}

func (f *rotatingFake) StartCredentialRotation(context.Context, string) (Access, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ca++
	return f.access(), nil
}

func (f *rotatingFake) CompleteCredentialRotation(context.Context, string) (Access, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access(), nil
}

func (f *rotatingFake) PlaceNodes(_ context.Context, _, pool string, zones []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones[pool] = zones
	return nil
}

func (f *rotatingFake) SetEmulatedVersion(_ context.Context, _, minor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emulated = minor
	return nil
}

func newMethodsServer(t *testing.T) (*Server, *rotatingFake) {
	t.Helper()
	f := &rotatingFake{fakeBackend: newFake(), zones: map[string][]string{}}
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", State: filepath.Join(t.TempDir(), "gke.json"),
		Versions: []Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}, AutoUpgradeInterval: -1, AutoscaleInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, f
}

// TestRotationLocationsAndUpgradeInfo: StartIPRotation changes the CA and
// refuses a second start; CompleteIPRotation ends it and refuses a second;
// a rotation seven days old completes by itself; SetLocations' rules and
// node counts; the upgrade info of a cluster on the newest version.
func TestRotationLocationsAndUpgradeInfo(t *testing.T) {
	s, f := newMethodsServer(t)
	ctx := context.Background()
	parent := "projects/p1/locations/us-central1-a"
	name := parent + "/clusters/c1"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1",
		NodePools: []*containerpb.NodePool{{Name: "pool", InitialNodeCount: 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)

	op, err = s.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name, RotateCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetMasterAuth().GetClusterCaCertificate() != "B" {
		t.Errorf("CA after the start = %q", c.GetMasterAuth().GetClusterCaCertificate())
	}
	if _, ok := s.Rotation(name); !ok {
		t.Error("no rotation in progress after the start")
	}
	_, err = s.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name})
	wantCode(t, "a second start", err, codes.FailedPrecondition)
	op, err = s.CompleteIPRotation(ctx, &containerpb.CompleteIPRotationRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	_, err = s.CompleteIPRotation(ctx, &containerpb.CompleteIPRotationRequest{Name: name})
	wantCode(t, "a second completion", err, codes.FailedPrecondition)

	// Seven days later a rotation completes by itself.
	op, _ = s.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name})
	wait(t, s, "p1", "us-central1-a", op)
	if _, err := s.AdvanceClock(7*24*time.Hour + time.Minute); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	ops := s.completeExpiredRotationsLocked(s.now())
	s.mu.Unlock()
	if len(ops) != 1 {
		t.Fatalf("auto-completed rotations = %d", len(ops))
	}
	wait(t, s, "p1", "us-central1-a", ops[0])
	if _, ok := s.Rotation(name); ok {
		t.Error("the rotation is still in progress after seven days")
	}

	// Zones.
	_, err = s.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-b"}})
	wantCode(t, "without the primary zone", err, codes.InvalidArgument)
	_, err = s.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-a", "europe-west1-b"}})
	wantCode(t, "another region", err, codes.InvalidArgument)
	_, err = s.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f", "us-central1-a"}})
	wantCode(t, "a zone twice", err, codes.InvalidArgument)
	_, err = s.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f", "us-central1-d"}})
	wantCode(t, "more nodes than a local cluster has", err, codes.ResourceExhausted)
	op, err = s.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-b", "us-central1-a"}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	kind := KindName("cloudburrow-t", name)
	if got := f.nodes[kind]["pool"]; got != 4 {
		t.Errorf("pool nodes across two zones = %d, want 4", got)
	}
	if len(c.GetLocations()) != 2 || len(c.GetNodePools()[0].GetLocations()) != 2 || len(f.zones["pool"]) != 2 {
		t.Errorf("locations %v, pool %v, placed %v", c.GetLocations(), c.GetNodePools()[0].GetLocations(), f.zones["pool"])
	}

	// Upgrade info.
	info, err := s.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.GetAutoUpgradeStatus()) != 1 || info.GetAutoUpgradeStatus()[0] != containerpb.ClusterUpgradeInfo_ACTIVE ||
		info.MinorTargetVersion != nil || info.PatchTargetVersion != nil {
		t.Errorf("upgrade info on the newest version = %v", info)
	}
	_, err = s.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	wantCode(t, "nothing to complete", err, codes.FailedPrecondition)
}

// TestUpgradePauses: the paused reasons and statuses of a maintenance
// policy, and the targets of the versions served.
func TestUpgradePauses(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	excl := func(scope containerpb.MaintenanceExclusionOptions_Scope) *containerpb.MaintenancePolicy {
		return &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{MaintenanceExclusions: map[string]*containerpb.TimeWindow{
			"x": {StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now.Add(time.Hour)),
				Options: &containerpb.TimeWindow_MaintenanceExclusionOptions{MaintenanceExclusionOptions: &containerpb.MaintenanceExclusionOptions{Scope: scope}}}}}}
	}
	for _, tc := range []struct {
		mp     *containerpb.MaintenancePolicy
		node   bool
		status containerpb.ClusterUpgradeInfo_AutoUpgradeStatus
	}{
		{nil, false, containerpb.ClusterUpgradeInfo_ACTIVE},
		{excl(containerpb.MaintenanceExclusionOptions_NO_UPGRADES), false, containerpb.ClusterUpgradeInfo_UPGRADE_PAUSED},
		{excl(containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES), false, containerpb.ClusterUpgradeInfo_MINOR_UPGRADE_PAUSED},
		{excl(containerpb.MaintenanceExclusionOptions_NO_MINOR_OR_NODE_UPGRADES), false, containerpb.ClusterUpgradeInfo_MINOR_UPGRADE_PAUSED},
		{excl(containerpb.MaintenanceExclusionOptions_NO_MINOR_OR_NODE_UPGRADES), true, containerpb.ClusterUpgradeInfo_UPGRADE_PAUSED},
	} {
		_, minor, all := pauses(tc.mp, now, tc.node)
		if got := upgradeStatus(minor, all); got != tc.status {
			t.Errorf("%v (node %v): %v, want %v", tc.mp, tc.node, got, tc.status)
		}
	}
	s := &Server{opts: Options{Versions: []Version{{GKE: "1.36.4-gke.0"}, {GKE: "1.35.9-gke.0"}, {GKE: "1.35.2-gke.0"}}}}
	if minor, patch := s.targets("1.35.2-gke.0"); minor != "1.36.4-gke.0" || patch != "1.35.9-gke.0" {
		t.Errorf("targets = %s, %s", minor, patch)
	}
}

// TestRollbackSafeUpgrade: step one emulates the previous minor version,
// the upgrade info reports it, and CompleteControlPlaneUpgrade (or the end
// of the soak) drops the emulation.
func TestRollbackSafeUpgrade(t *testing.T) {
	f := &rotatingFake{fakeBackend: newFake(), zones: map[string][]string{}}
	b := &upgradingFake{rotatingFake: f}
	s, err := NewServer(b, Options{Instance: "cloudburrow-t", AutoUpgradeInterval: -1, AutoscaleInterval: -1,
		Versions: []Version{{GKE: "1.36.4-gke.0", Image: "new"}, {GKE: "1.35.2-gke.0", Image: "old"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	parent := "projects/p1/locations/us-central1-a"
	name := parent + "/clusters/c1"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialClusterVersion: "1.35.2-gke.0",
		NodePools: []*containerpb.NodePool{{Name: "pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.36.4-gke.0",
		DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{ControlPlaneSoakDuration: durationpb.New(8 * 24 * time.Hour)}}})
	wantCode(t, "an eight-day soak", err, codes.InvalidArgument)
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.36.4-gke.0",
		DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentMasterVersion() != "1.36.4-gke.0" || c.GetCurrentEmulatedVersion() != "1.35.2-gke.0" || f.emulated != "1.35" {
		t.Errorf("after step one: master %s, emulated %s, backend %q", c.GetCurrentMasterVersion(), c.GetCurrentEmulatedVersion(), f.emulated)
	}
	info, _ := s.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	rs := info.GetRollbackSafeUpgradeStatus()
	if rs.GetMode() != containerpb.RollbackSafeUpgradeStatus_KCP_MINOR_UPGRADE_ROLLBACK_SAFE_MODE || rs.GetPreviousVersion() != "1.35.2-gke.0" {
		t.Errorf("rollback-safe status %v", rs)
	}
	if d := info.GetUpgradeDetails(); len(d) != 1 || d[0].GetState() != containerpb.UpgradeDetails_SUCCEEDED || d[0].GetTargetVersion() != "1.36.4-gke.0" {
		t.Errorf("upgrade details %v", d)
	}
	// The soak's end completes it: moving the clock starts the automatic
	// work now due.
	if _, err := s.AdvanceClock(defaultRollbackSoak + time.Minute); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); c.GetStatus() == containerpb.Cluster_RUNNING && c.GetCurrentEmulatedVersion() == "1.36.4-gke.0" {
			break
		}
	}
	if c.GetCurrentEmulatedVersion() != "1.36.4-gke.0" || f.emulated != "" {
		t.Errorf("after step two: emulated %s, backend %q", c.GetCurrentEmulatedVersion(), f.emulated)
	}
	_, err = s.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	wantCode(t, "completing twice", err, codes.FailedPrecondition)
}

// upgradingFake upgrades too.
type upgradingFake struct {
	*rotatingFake
	rolledBack string
}

func (*upgradingFake) UpgradeControlPlane(context.Context, string, string) error { return nil }
func (*upgradingFake) PoolNodes(context.Context, string, string) ([]Node, error) { return nil, nil }
func (*upgradingFake) AddNode(context.Context, string, string, string) (string, error) {
	return "n", nil
}
func (*upgradingFake) CordonNode(context.Context, string, string) error     { return nil }
func (*upgradingFake) UncordonNode(context.Context, string, string) error   { return nil }
func (*upgradingFake) DrainNode(context.Context, string, string) error      { return nil }
func (*upgradingFake) ForceDrainNode(context.Context, string, string) error { return nil }
func (*upgradingFake) RemoveNode(context.Context, string, string) error     { return nil }

func (u *upgradingFake) RollbackControlPlane(_ context.Context, _, image string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rolledBack = image
	return nil
}

// TestRollbackSafeRollback: during the soak the control plane goes back to
// the exact previous version, and nowhere else; nodes are upgraded no
// further than the emulated version; after the rollback there is no
// rollback-safe status, and a downgrade is refused.
func TestRollbackSafeRollback(t *testing.T) {
	f := &rotatingFake{fakeBackend: newFake(), zones: map[string][]string{}}
	b := &upgradingFake{rotatingFake: f}
	s, err := NewServer(b, Options{Instance: "cloudburrow-t", AutoUpgradeInterval: -1, AutoscaleInterval: -1,
		Versions: []Version{{GKE: "1.36.4-gke.0", Image: "new"}, {GKE: "1.35.3-gke.0", Image: "old3"}, {GKE: "1.35.2-gke.0", Image: "old"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	parent := "projects/p1/locations/us-central1-a"
	name := parent + "/clusters/c1"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialClusterVersion: "1.35.2-gke.0",
		NodePools: []*containerpb.NodePool{{Name: "pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.36.4-gke.0",
		DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.35.3-gke.0"}})
	wantCode(t, "rolling back to another patch", err, codes.InvalidArgument)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredNodeVersion: "1.36.4-gke.0", DesiredNodePoolId: "pool"}})
	wantCode(t, "a node upgrade past the emulated version", err, codes.FailedPrecondition)
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.35.2-gke.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_MASTER {
		t.Errorf("the rollback is %s", op.GetOperationType())
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentMasterVersion() != "1.35.2-gke.0" || c.GetCurrentEmulatedVersion() != "1.35.2-gke.0" || b.rolledBack != "old" {
		t.Errorf("after the rollback: master %s, emulated %s, backend %q", c.GetCurrentMasterVersion(), c.GetCurrentEmulatedVersion(), b.rolledBack)
	}
	info, _ := s.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if info.GetRollbackSafeUpgradeStatus() != nil {
		t.Errorf("a rollback-safe status after the rollback: %v", info.GetRollbackSafeUpgradeStatus())
	}
	if d := info.GetUpgradeDetails(); len(d) != 2 || d[1].GetInitialVersion() != "1.36.4-gke.0" || d[1].GetTargetVersion() != "1.35.2-gke.0" {
		t.Errorf("upgrade details %v", d)
	}
	_, err = s.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	wantCode(t, "completing after a rollback", err, codes.FailedPrecondition)
	// Upgraded again, then completed: no way back.
	op, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.36.4-gke.0",
		DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	op, err = s.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	_, err = s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: "1.35.2-gke.0"}})
	wantCode(t, "a downgrade after step two", err, codes.InvalidArgument)
}
