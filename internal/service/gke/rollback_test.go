package gke

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

// images is a pool's nodes' images, in order.
func (f *upFake) images(pool string) []string {
	f.umu.Lock()
	defer f.umu.Unlock()
	var out []string
	for _, n := range f.pools[pool] {
		out = append(out, n.Image)
	}
	return out
}

// onFirst returns a hook that, on the first call of verb, signals
// reached and waits for release.
func onFirst(verb string) (hook func(string, string), reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	return func(v, _ string) {
		if v != verb {
			return
		}
		once.Do(func() {
			close(reached)
			<-release
		})
	}, reached, release
}

func upgradeMasterTo(t *testing.T, s *Server, name string) {
	t.Helper()
	op, err := s.UpdateMaster(context.Background(), &containerpb.UpdateMasterRequest{Name: name, MasterVersion: newV})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1-a", op)
}

func TestRollbackCancelledSurgeUpgrade(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	name := makeOld(t, s, "r1", 3, nil, nil)
	upgradeMasterTo(t, s, name)
	pool := name + "/nodePools/default-pool"

	// Nothing to roll back yet.
	_, err := s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantCode(t, "rollback with no upgrade", err, codes.FailedPrecondition)

	hook, reached, release := onFirst("add")
	f.setHook(hook)
	op, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	// A running surge upgrade is cancelled before it is rolled back.
	_, err = s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantCode(t, "rollback of a running surge upgrade", err, codes.FailedPrecondition)
	if _, err := s.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: opName("p", "us-central1-a", op.Name)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetOperation(ctx, &containerpb.GetOperationRequest{Name: opName("p", "us-central1-a", op.Name)})
	if got.Status != containerpb.Operation_ABORTING {
		t.Errorf("a cancelled upgrade's status = %s, want ABORTING", got.Status)
	}
	close(release)
	got = wait(t, s, "p", "us-central1-a", op)
	if codes.Code(got.GetError().GetCode()) != codes.Canceled {
		t.Errorf("cancelled upgrade's error = %v", got.GetError())
	}
	// The node that had started completed; the others did not start.
	if im := f.images("default-pool"); strings.Count(strings.Join(im, ","), newImg) != 1 || len(im) != 3 {
		t.Fatalf("after cancel the pool's images = %v", im)
	}
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.Version != oldV || np.Status != containerpb.NodePool_RUNNING {
		t.Errorf("after cancel: version %s, status %s", np.Version, np.Status)
	}

	f.setHook(nil)
	rb, err := s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	if rb.OperationType != containerpb.Operation_UPGRADE_NODES || rb.Status != containerpb.Operation_RUNNING {
		t.Errorf("rollback operation = %v", rb)
	}
	if got := wait(t, s, "p", "us-central1-a", rb); got.Error != nil {
		t.Fatal(got.Error)
	}
	for _, im := range f.images("default-pool") {
		if im != oldImg {
			t.Errorf("after rollback the pool's images = %v", f.images("default-pool"))
			break
		}
	}
	if n := len(f.images("default-pool")); n != 3 {
		t.Errorf("after rollback the pool has %d nodes", n)
	}
	// respect_pdb unset: the upgraded node is drained without evictions.
	if e := f.events(); !strings.Contains(e, "force-drain n4") || !strings.Contains(e, "remove n4") {
		t.Errorf("events = %s", e)
	}
	np, _ = s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.Version != oldV || np.Status != containerpb.NodePool_RUNNING || np.StatusMessage != "" {
		t.Errorf("after rollback: %v", np)
	}

	// A completed upgrade cannot be rolled back.
	op, _ = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	wait(t, s, "p", "us-central1-a", op)
	_, err = s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantCode(t, "rollback of a completed upgrade", err, codes.FailedPrecondition)
}

func TestRollbackBlueGreenWhileSoakingRespectsPDB(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	bg := containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	name := makeOld(t, s, "r2", 2, nil, &containerpb.NodePool_UpgradeSettings{Strategy: &bg,
		BlueGreenSettings: &containerpb.BlueGreenSettings{NodePoolSoakDuration: durationpb.New(time.Hour)}})
	upgradeMasterTo(t, s, name)
	pool := name + "/nodePools/default-pool"
	op, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, pool, containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING)
	rb, err := s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool, RespectPdb: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, s, "p", "us-central1-a", op); codes.Code(got.GetError().GetCode()) != codes.Canceled {
		t.Errorf("the rolled-back upgrade ended %v", got)
	}
	if got := wait(t, s, "p", "us-central1-a", rb); got.Error != nil {
		t.Fatal(got.Error)
	}
	// The blue nodes (n1, n2) are uncordoned; the green ones (n3, n4)
	// cordoned, drained through evictions and deleted.
	e := f.events()
	for _, w := range []string{"uncordon n1", "uncordon n2", "cordon n3", "drain n3", "remove n3", "drain n4", "remove n4"} {
		if !strings.Contains(e, w) {
			t.Errorf("events lack %q: %s", w, e)
		}
	}
	if strings.Contains(e, "remove n1") || strings.Contains(e, "force-drain") {
		t.Errorf("events = %s", e)
	}
	if im := f.images("default-pool"); len(im) != 2 || im[0] != oldImg || im[1] != oldImg {
		t.Errorf("after rollback the pool's images = %v", im)
	}
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.Version != oldV || np.UpdateInfo != nil || np.Status != containerpb.NodePool_RUNNING {
		t.Errorf("after rollback: %v", np)
	}
}

func TestBlueGreenDeletingBluePoolCannotBeCancelledOrRolledBack(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	bg := containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	name := makeOld(t, s, "r3", 1, nil, &containerpb.NodePool_UpgradeSettings{Strategy: &bg,
		BlueGreenSettings: &containerpb.BlueGreenSettings{NodePoolSoakDuration: durationpb.New(0)}})
	upgradeMasterTo(t, s, name)
	pool := name + "/nodePools/default-pool"
	hook, reached, release := onFirst("remove")
	f.setHook(hook)
	op, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	_, err = s.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: opName("p", "us-central1-a", op.Name)})
	wantCode(t, "cancel in DELETING_BLUE_POOL", err, codes.FailedPrecondition)
	_, err = s.RollbackNodePoolUpgrade(ctx, &containerpb.RollbackNodePoolUpgradeRequest{Name: pool})
	wantCode(t, "rollback in DELETING_BLUE_POOL", err, codes.FailedPrecondition)
	close(release)
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
}

func TestBlueGreenCancelThenResumeKeepsTheGreenNodes(t *testing.T) {
	s, f := newUpServer(t, nil)
	ctx := context.Background()
	bg := containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	name := makeOld(t, s, "r4", 2, nil, &containerpb.NodePool_UpgradeSettings{Strategy: &bg,
		BlueGreenSettings: &containerpb.BlueGreenSettings{NodePoolSoakDuration: durationpb.New(time.Hour)}})
	upgradeMasterTo(t, s, name)
	pool := name + "/nodePools/default-pool"
	op, _ := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	waitPhase(t, s, pool, containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING)
	if _, err := s.CancelOperation(ctx, &containerpb.CancelOperationRequest{Name: opName("p", "us-central1-a", op.Name)}); err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1-a", op)
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() != containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING || np.Version != oldV {
		t.Errorf("a cancelled blue-green upgrade is not paused in its phase: %v", np)
	}
	// Resumed by upgrading again: no second green pool.
	op, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: newV, ImageType: "COS_CONTAINERD"})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, pool, containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING)
	if _, err := s.CompleteNodePoolUpgrade(ctx, &containerpb.CompleteNodePoolUpgradeRequest{Name: pool}); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	if im := f.images("default-pool"); len(im) != 2 || im[0] != newImg || im[1] != newImg {
		t.Errorf("after the resumed upgrade the pool's images = %v", im)
	}
	// "The upgrade process will continue at the intermediate phase it was
	// paused": the resumed upgrade starts soaking, without cordoning or
	// draining the blue nodes again.
	e := ", " + f.events() + ","
	for _, w := range []string{"cordon n1", "drain n1", "cordon n2", "drain n2"} {
		if c := strings.Count(e, ", "+w+","); c != 1 {
			t.Errorf("%q happened %d times: %s", w, c, e)
		}
	}
}

func waitPhase(t *testing.T, s *Server, pool string, ph containerpb.NodePool_UpdateInfo_BlueGreenInfo_Phase) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		np, _ := s.GetNodePool(context.Background(), &containerpb.GetNodePoolRequest{Name: pool})
		if np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() == ph {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never %s: %v", ph, np.GetUpdateInfo())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAutoUpgradePausesPastTheWindowAndResumes(t *testing.T) {
	base := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC) // inside 03:00–07:00
	s, f := newUpServer(t, func() time.Time { return base })
	ctx := context.Background()
	mp := &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
		Policy: &containerpb.MaintenanceWindow_DailyMaintenanceWindow{DailyMaintenanceWindow: &containerpb.DailyMaintenanceWindow{StartTime: "03:00"}}}}
	name := makeOld(t, s, "r5", 3, mp, nil)
	upgradeMasterTo(t, s, name) // manual: ignores the window
	pool := name + "/nodePools/default-pool"

	// The window ends (07:00) while the first node is upgraded.
	f.setHook(func(v, _ string) {
		if v == "add" && len(f.images("default-pool")) == 3 {
			if _, err := s.AdvanceClock(4 * time.Hour); err != nil {
				t.Error(err)
			}
		}
	})
	ops := s.AutoUpgrade()
	if len(ops) != 1 || ops[0].OperationType != containerpb.Operation_UPGRADE_NODES {
		t.Fatalf("auto-upgrade = %v", ops)
	}
	got := wait(t, s, "p", "us-central1-a", ops[0])
	if got.Error != nil || got.StatusMessage != pausedMessage {
		t.Fatalf("an upgrade past its window ended %v", got)
	}
	f.setHook(nil)
	if im := strings.Join(f.images("default-pool"), ","); strings.Count(im, newImg) != 1 {
		t.Fatalf("paused with images %s, want one node upgraded", im)
	}
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.Version != oldV || np.StatusMessage != pausedMessage {
		t.Errorf("paused pool = %v", np)
	}
	// Outside the window nothing resumes.
	if ops := s.AutoUpgrade(); len(ops) != 0 {
		t.Fatalf("resumed outside the window: %v", ops)
	}
	// The next window (03:00 tomorrow): the clock's advance resumes it.
	if _, err := s.AdvanceClock(20 * time.Hour); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		np, _ = s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
		if np.Version == newV && np.Status == containerpb.NodePool_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never resumed: %v", np)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, im := range f.images("default-pool") {
		if im != newImg {
			t.Errorf("after the resumed upgrade: %v", f.images("default-pool"))
			break
		}
	}
	if s.Now().Sub(base) != 24*time.Hour {
		t.Errorf("clock = %s", s.Now())
	}
	_, err := s.AdvanceClock(-time.Second)
	if err == nil {
		t.Error("the clock moved back")
	}
}
