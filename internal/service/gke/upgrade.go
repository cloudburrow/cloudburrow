package gke

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Cluster upgrades (#2069), as GKE does them (re-read 2026-11-20:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster,
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pool-upgrade-strategies,
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/maintenance-windows-and-exclusions):
//
//   - the control plane first (UPGRADE_MASTER), one minor version at a
//     time; then each node pool (UPGRADE_NODES), never newer than the
//     control plane;
//   - a node pool's nodes are replaced per its upgradeSettings: surge
//     (maxSurge new nodes are added and Ready before as many old ones are
//     cordoned, drained and deleted; maxUnavailable old ones are cordoned,
//     drained and recreated without a surge node), or blue-green (the green
//     nodes are made, the blue ones cordoned, drained in batches with a
//     soak between batches, the pool soaked, then the blue nodes deleted);
//   - manual upgrades start at once and ignore maintenance windows and
//     exclusions; automatic upgrades (to the release channel's target) run
//     only inside the maintenance window, if one is set, and outside every
//     exclusion whose scope covers them.
//
// The Backend does the node work; a Backend that is not an Upgrader serves
// no upgrade across versions.

// Upgrader is what a Backend does for upgrades.
type Upgrader interface {
	// UpgradeControlPlane moves the control plane to image's version,
	// keeping the cluster's state.
	UpgradeControlPlane(ctx context.Context, kind, image string) error
	// PoolNodes lists a pool's nodes, with the node image each runs.
	PoolNodes(ctx context.Context, kind, pool string) ([]Node, error)
	// AddNode joins a node of image to pool and returns its name when it
	// is Ready.
	AddNode(ctx context.Context, kind, image, pool string) (string, error)
	CordonNode(ctx context.Context, kind, name string) error
	UncordonNode(ctx context.Context, kind, name string) error
	// DrainNode evicts a node's pods, respecting PodDisruptionBudgets for
	// up to an hour.
	DrainNode(ctx context.Context, kind, name string) error
	// ForceDrainNode deletes a node's pods without the eviction API, so
	// PodDisruptionBudgets are not respected (a rollback's default).
	ForceDrainNode(ctx context.Context, kind, name string) error
	// RemoveNode deletes the node and its machine.
	RemoveNode(ctx context.Context, kind, name string) error
}

// DefaultNodePoolSoakDuration is a blue-green upgrade's default pool soak:
// one hour.
const DefaultNodePoolSoakDuration = time.Hour

// maxSoak is the longest total soak, after which the blue pool is deleted.
const maxSoak = 7 * 24 * time.Hour

func (s *Server) upgrader() (Upgrader, error) {
	u, ok := s.backend.(Upgrader)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Upgrading across Kubernetes versions is not available with this backend.")
	}
	return u, nil
}

// ---- versions ----

// versionParts is X.Y.Z of X.Y.Z-gke.N.
func versionParts(v string) [3]int {
	var out [3]int
	base := v
	if i := strings.IndexAny(base, "-+"); i >= 0 {
		base = base[:i]
	}
	for i, p := range strings.SplitN(base, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// compareVersions is -1, 0 or 1.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func minorOf(v string) int { return versionParts(v)[1] }

func minorDiffers(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	return pa[0] != pb[0] || pa[1] != pb[1]
}

// maxNodeSkew is how many minor versions a node may be behind the control
// plane (GKE follows Kubernetes' version skew policy).
const maxNodeSkew = 2

// checkMasterUpgrade refuses a control-plane version GKE refuses.
func checkMasterUpgrade(c *containerpb.Cluster, to Version) error {
	from := c.GetCurrentMasterVersion()
	if compareVersions(to.GKE, from) < 0 {
		// UNVERIFIED wording (re-read 2026-11-20: upgrading-a-cluster): the
		// control plane cannot be downgraded across minor versions.
		return status.Errorf(codes.InvalidArgument, "Master version %q is older than the current version %q: the control plane cannot be downgraded.", to.GKE, from)
	}
	if minorOf(to.GKE) > minorOf(from)+1 {
		// Documented: the control plane is upgraded one minor version at a
		// time. UNVERIFIED wording (re-read 2026-11-20: upgrading-a-cluster).
		return status.Errorf(codes.InvalidArgument, "Master version %q is more than one minor version newer than %q: upgrade one minor version at a time.", to.GKE, from)
	}
	for _, np := range c.GetNodePools() {
		if minorOf(to.GKE)-minorOf(np.GetVersion()) > maxNodeSkew {
			return status.Errorf(codes.FailedPrecondition, "Node pool %q runs %s, which would be more than %d minor versions older than the control plane: upgrade it first.", np.GetName(), np.GetVersion(), maxNodeSkew)
		}
	}
	return nil
}

// checkNodeUpgrade refuses a node version GKE refuses.
func checkNodeUpgrade(c *containerpb.Cluster, to Version) error {
	if compareVersions(to.GKE, c.GetCurrentMasterVersion()) > 0 {
		// Documented: "node pools cannot run a newer version than the
		// control plane". UNVERIFIED wording (re-read 2026-11-20: upgrading-a-cluster).
		return status.Errorf(codes.InvalidArgument, "Node version %q is newer than the control plane's %q: upgrade the control plane first.", to.GKE, c.GetCurrentMasterVersion())
	}
	if minorOf(c.GetCurrentMasterVersion())-minorOf(to.GKE) > maxNodeSkew {
		return status.Errorf(codes.InvalidArgument, "Node version %q is more than %d minor versions older than the control plane's %q.", to.GKE, maxNodeSkew, c.GetCurrentMasterVersion())
	}
	return nil
}

// resolveNodeVersion is resolveVersion, with "-" the control plane's
// version, as the API documents for node versions.
func (s *Server) resolveNodeVersion(c *containerpb.Cluster, v string) (Version, error) {
	if v == "-" {
		v = c.GetCurrentMasterVersion()
	}
	return s.resolveVersion("Node version", v)
}

// ---- upgrade settings ----

// defaultUpgradeSettings are a new pool's: surge, one node at a time.
func defaultUpgradeSettings() *containerpb.NodePool_UpgradeSettings {
	st := containerpb.NodePoolUpdateStrategy_SURGE
	return &containerpb.NodePool_UpgradeSettings{MaxSurge: 1, Strategy: &st}
}

// checkUpgradeSettings refuses settings GKE refuses: a surge upgrade needs
// maxSurge or maxUnavailable, and their sum is at most 100 (re-read
// 2026-11-20: node-pool-upgrade-strategies).
func checkUpgradeSettings(u *containerpb.NodePool_UpgradeSettings) error {
	if u == nil {
		return nil
	}
	switch u.GetStrategy() {
	case containerpb.NodePoolUpdateStrategy_NODE_POOL_UPDATE_STRATEGY_UNSPECIFIED, containerpb.NodePoolUpdateStrategy_SURGE:
		if u.GetMaxSurge() < 0 || u.GetMaxUnavailable() < 0 {
			return status.Error(codes.InvalidArgument, "upgrade_settings.max_surge and max_unavailable must not be negative.")
		}
		if u.GetMaxSurge() == 0 && u.GetMaxUnavailable() == 0 {
			return status.Error(codes.InvalidArgument, "upgrade_settings: max_surge and max_unavailable cannot both be 0.")
		}
		if u.GetMaxSurge()+u.GetMaxUnavailable() > 100 {
			return status.Error(codes.InvalidArgument, "upgrade_settings: max_surge + max_unavailable must be at most 100.")
		}
	case containerpb.NodePoolUpdateStrategy_BLUE_GREEN:
		p := u.GetBlueGreenSettings().GetStandardRolloutPolicy()
		if p.GetBatchPercentage() < 0 || p.GetBatchPercentage() > 1 {
			return status.Error(codes.InvalidArgument, "blue_green_settings.standard_rollout_policy.batch_percentage must be between 0 and 1.")
		}
		if p.GetBatchNodeCount() < 0 {
			return status.Error(codes.InvalidArgument, "blue_green_settings.standard_rollout_policy.batch_node_count must not be negative.")
		}
	default:
		return status.Errorf(codes.Unimplemented, "Node pool upgrade strategy %s is not available locally.", u.GetStrategy())
	}
	return nil
}

// ---- progress ----

type opKey struct{}

// progress changes the running operation's record, under s.mu.
func (s *Server) progress(ctx context.Context, f func(op *containerpb.Operation)) {
	op, _ := ctx.Value(opKey{}).(*containerpb.Operation)
	if op == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f(op)
	_ = s.saveLocked()
}

func intMetric(name string, v int) *containerpb.OperationProgress_Metric {
	return &containerpb.OperationProgress_Metric{Name: name, Value: &containerpb.OperationProgress_Metric_IntValue{IntValue: int64(v)}}
}

// nodeProgress records a node upgrade's progress: the nodes upgraded of
// the pool's. The metric names are UNVERIFIED (re-read 2026-11-20:
// node-pool-upgrade-strategies names none).
func (s *Server) nodeProgress(ctx context.Context, pool string, done, total int, stage string) {
	s.progress(ctx, func(op *containerpb.Operation) {
		op.Detail = fmt.Sprintf("Upgrading %s: %d of %d nodes upgraded. %s", pool, done, total, stage)
		op.Progress = &containerpb.OperationProgress{Name: pool, Status: containerpb.Operation_RUNNING,
			Metrics: []*containerpb.OperationProgress_Metric{intMetric("NODES_TOTAL", total), intMetric("NODES_DONE", done)}}
	})
}

// ---- control plane ----

// startMasterUpgradeLocked upgrades e's control plane to to; s.mu is held
// and the cluster is ready.
func (s *Server) startMasterUpgradeLocked(p parsed, e *entry, to Version, u Upgrader) *containerpb.Operation {
	c := e.cluster
	c.Status = containerpb.Cluster_RECONCILING
	kind := e.kind
	autopilot := c.GetAutopilot().GetEnabled()
	return s.startOpLocked(p, containerpb.Operation_UPGRADE_MASTER, c.SelfLink, e, func(ctx context.Context) error {
		s.progress(ctx, func(op *containerpb.Operation) {
			op.Detail = "Upgrading the control plane to " + to.GKE + "."
			op.Progress = &containerpb.OperationProgress{Name: "control-plane", Status: containerpb.Operation_RUNNING}
		})
		if err := u.UpgradeControlPlane(ctx, kind, to.Image); err != nil {
			return status.Errorf(codes.Internal, "Upgrading the control plane to %s failed: %v", to.GKE, err)
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		c.CurrentMasterVersion = to.GKE
		if autopilot {
			// Google manages an Autopilot cluster's nodes: the local
			// cluster's one node is the control plane's, upgraded with it.
			c.CurrentNodeVersion = to.GKE
		}
	})
}

// ---- node pools ----

// startPoolUpgradeLocked replaces pool np's nodes with nodes of to, per
// its upgrade settings; s.mu is held and the cluster is ready. auto is an
// automatic upgrade's: a surge one pauses when it runs past the end of the
// maintenance window.
func (s *Server) startPoolUpgradeLocked(p parsed, e *entry, np *containerpb.NodePool, to Version, u Upgrader, auto bool) *containerpb.Operation {
	c := e.cluster
	c.Status = containerpb.Cluster_RECONCILING
	np.Status = containerpb.NodePool_RECONCILING
	settings := proto.Clone(np.GetUpgradeSettings()).(*containerpb.NodePool_UpgradeSettings)
	if np.GetUpgradeSettings() == nil {
		settings = defaultUpgradeSettings()
	}
	zones := len(np.Locations)
	if zones == 0 {
		zones = 1
	}
	kind, pool := e.kind, np.Name
	// A cancelled blue-green upgrade to the same version resumes "at the
	// intermediate phase it was paused" (re-read 2026-11-21:
	// concepts/node-pool-upgrade-strategies): read that phase now, while
	// s.mu is held, before the operation's goroutine rewrites it.
	resume := containerpb.NodePool_UpdateInfo_BlueGreenInfo_CREATING_GREEN_POOL
	if bgi := np.GetUpdateInfo().GetBlueGreenInfo(); bgi.GetGreenPoolVersion() == to.GKE &&
		bgi.GetPhase() > resume && bgi.GetPhase() <= containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING {
		resume = bgi.GetPhase()
	}
	soak := make(chan struct{})
	e.soak = map[string]chan struct{}{pool: soak}
	pause := make(chan struct{})
	// stop is checked between steps: a cancelled upgrade stops (the nodes
	// it started complete), and an automatic surge upgrade past the end of
	// its window pauses, to resume in the next one (re-read 2026-11-20:
	// concepts/maintenance-windows-and-exclusions, "If tasks run beyond
	// the maintenance window, GKE attempts to pause the tasks", and
	// how-to/upgrading-a-cluster, rollback when "the upgrade is incomplete
	// due to a maintenance window timing out").
	stop := func(window bool) error {
		select {
		case <-pause:
			return errCancelled
		default:
		}
		if auto && window {
			s.mu.Lock()
			mp := e.cluster.GetMaintenancePolicy()
			s.mu.Unlock()
			if !inWindow(mp, s.now()) {
				return errPaused
			}
		}
		return nil
	}
	op := s.startOpLocked(p, containerpb.Operation_UPGRADE_NODES, np.SelfLink, e, func(ctx context.Context) error {
		nodes, err := u.PoolNodes(ctx, kind, pool)
		if err != nil {
			return status.Errorf(codes.Internal, "Listing node pool %q's nodes failed: %v", pool, err)
		}
		var old []string
		green := 0
		for _, n := range nodes {
			if n.Image != to.Image {
				old = append(old, n.Name)
			} else {
				green++
			}
		}
		if settings.GetStrategy() == containerpb.NodePoolUpdateStrategy_BLUE_GREEN {
			// "Blue-green upgrades continue until completion if they
			// exceed a maintenance window" (node-pool-upgrade-strategies):
			// only a cancel stops one. A resumed one keeps the green nodes
			// it made and starts at the phase it was paused in.
			return s.blueGreen(ctx, u, np, kind, pool, to, old, green, resume, settings.GetBlueGreenSettings(), soak, pause, func() error { return stop(false) })
		}
		return s.surge(ctx, u, kind, pool, to, old, zones, int(settings.GetMaxSurge()), int(settings.GetMaxUnavailable()), func() error { return stop(true) })
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		delete(e.soak, pool)
		switch {
		case errors.Is(err, errPaused):
			// Partly upgraded: the pool keeps its version until every node
			// runs the new one.
			np.Status = containerpb.NodePool_RUNNING
			np.StatusMessage = pausedMessage
			return
		case status.Code(err) == codes.Canceled:
			// A cancelled blue-green upgrade stays in its phase, to be
			// resumed or rolled back.
			np.Status = containerpb.NodePool_RUNNING
			np.StatusMessage = "The upgrade to " + to.GKE + " was cancelled: resume it by upgrading again, or roll it back."
			if np.GetUpdateInfo().GetBlueGreenInfo() == nil {
				np.UpdateInfo = nil
			}
			return
		}
		np.UpdateInfo = nil
		if err != nil {
			np.Status = containerpb.NodePool_RUNNING_WITH_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		np.Status = containerpb.NodePool_RUNNING
		np.StatusMessage = ""
		np.Version = to.GKE
		c.CurrentNodeVersion = oldestNodeVersion(c)
	})
	if oe := s.ops[opName(p.project, p.location, op.Name)]; oe != nil {
		oe.pause, oe.pool = pause, np
	}
	return op
}

// errPaused ends an automatic upgrade that ran past the end of its
// maintenance window: done, the pool partly upgraded.
var errPaused = errors.New("paused at the end of the maintenance window")

// pausedMessage is a paused upgrade's operation and node pool message.
// UNVERIFIED wording (re-read 2026-11-20: maintenance-windows-and-exclusions
// gives none).
const pausedMessage = "The automatic upgrade ran past the end of the maintenance window and was paused: it resumes in the next maintenance window."

// errCancelled ends a node pool upgrade that CancelOperation stopped.
var errCancelled = status.Error(codes.Canceled, "Operation was cancelled.")

// oldestNodeVersion is the cluster's node version: its oldest pool's.
func oldestNodeVersion(c *containerpb.Cluster) string {
	v := ""
	for _, np := range c.NodePools {
		if v == "" || compareVersions(np.Version, v) < 0 {
			v = np.Version
		}
	}
	if v == "" {
		return c.CurrentMasterVersion
	}
	return v
}

// surge upgrades old nodes zone by zone (a zone's nodes are consecutive),
// at most maxSurge+maxUnavailable of a zone at a time: maxSurge new nodes
// are added and Ready before as many old ones are drained and deleted; the
// rest of the batch is drained, deleted and then recreated.
func (s *Server) surge(ctx context.Context, u Upgrader, kind, pool string, to Version, old []string, zones, maxSurge, maxUnavailable int, stop func() error) error {
	total := len(old)
	done := 0
	per := int(math.Ceil(float64(total) / float64(zones)))
	if per == 0 {
		per = 1
	}
	s.nodeProgress(ctx, pool, 0, total, "")
	for z := 0; z < total; z += per {
		zone := old[z:min(z+per, total)]
		for len(zone) > 0 {
			if done > 0 {
				if err := stop(); err != nil {
					return err
				}
			}
			batch := zone[:min(maxSurge+maxUnavailable, len(zone))]
			zone = zone[len(batch):]
			surgeN := min(maxSurge, len(batch))
			for i := 0; i < surgeN; i++ {
				if _, err := u.AddNode(ctx, kind, to.Image, pool); err != nil {
					return status.Errorf(codes.Internal, "Adding a surge node to %q failed: %v", pool, err)
				}
			}
			for _, n := range batch {
				if err := u.CordonNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Cordoning node %s failed: %v", n, err)
				}
			}
			for _, n := range batch {
				s.nodeProgress(ctx, pool, done, total, "Draining "+n+".")
				if err := u.DrainNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Draining node %s failed: %v", n, err)
				}
				if err := u.RemoveNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Deleting node %s failed: %v", n, err)
				}
			}
			for i := surgeN; i < len(batch); i++ {
				if _, err := u.AddNode(ctx, kind, to.Image, pool); err != nil {
					return status.Errorf(codes.Internal, "Recreating a node of %q failed: %v", pool, err)
				}
			}
			done += len(batch)
			s.nodeProgress(ctx, pool, done, total, "")
		}
	}
	return nil
}

// blueGreen makes the green nodes, cordons the blue ones, drains them in
// batches, soaks the pool (CompleteNodePoolUpgrade ends it early) and
// deletes the blue nodes. It starts at phase from: a resumed upgrade skips
// the phases it finished before it was cancelled, so it never reports an
// earlier phase than the one it was paused in.
func (s *Server) blueGreen(ctx context.Context, u Upgrader, np *containerpb.NodePool, kind, pool string, to Version,
	blue []string, green int, from containerpb.NodePool_UpdateInfo_BlueGreenInfo_Phase,
	bg *containerpb.BlueGreenSettings, soak, pause chan struct{}, stop func() error) error {
	total := len(blue)
	phase := func(ph containerpb.NodePool_UpdateInfo_BlueGreenInfo_Phase, deletion string) {
		s.mu.Lock()
		info := &containerpb.NodePool_UpdateInfo_BlueGreenInfo{Phase: ph, GreenPoolVersion: to.GKE,
			BlueInstanceGroupUrls: np.GetInstanceGroupUrls(), GreenInstanceGroupUrls: np.GetInstanceGroupUrls(), BluePoolDeletionStartTime: deletion}
		np.UpdateInfo = &containerpb.NodePool_UpdateInfo{BlueGreenInfo: info}
		s.mu.Unlock()
		s.nodeProgress(ctx, pool, 0, total, "Phase: "+ph.String()+".")
	}
	if from <= containerpb.NodePool_UpdateInfo_BlueGreenInfo_CREATING_GREEN_POOL {
		phase(containerpb.NodePool_UpdateInfo_BlueGreenInfo_CREATING_GREEN_POOL, "")
		for i := green; i < total; i++ {
			if err := stop(); err != nil {
				return err
			}
			if _, err := u.AddNode(ctx, kind, to.Image, pool); err != nil {
				return status.Errorf(codes.Internal, "Creating the green pool failed: %v", err)
			}
		}
		if err := stop(); err != nil {
			return err
		}
	}
	if from <= containerpb.NodePool_UpdateInfo_BlueGreenInfo_CORDONING_BLUE_POOL {
		phase(containerpb.NodePool_UpdateInfo_BlueGreenInfo_CORDONING_BLUE_POOL, "")
		for _, n := range blue {
			if err := u.CordonNode(ctx, kind, n); err != nil {
				return status.Errorf(codes.Internal, "Cordoning node %s failed: %v", n, err)
			}
		}
		if err := stop(); err != nil {
			return err
		}
	}
	waited := time.Duration(0)
	if from <= containerpb.NodePool_UpdateInfo_BlueGreenInfo_DRAINING_BLUE_POOL {
		phase(containerpb.NodePool_UpdateInfo_BlueGreenInfo_DRAINING_BLUE_POOL, "")
		rp := bg.GetStandardRolloutPolicy()
		batch := total
		if n := int(rp.GetBatchNodeCount()); n > 0 {
			batch = n
		} else if f := rp.GetBatchPercentage(); f > 0 {
			batch = max(1, int(math.Ceil(float64(total)*float64(f))))
		}
		for i := 0; i < total; i += batch {
			if i > 0 {
				d := rp.GetBatchSoakDuration().AsDuration()
				if err := sleep(ctx, d, nil, pause); err != nil {
					return err
				}
				waited += d
			}
			if err := stop(); err != nil {
				return err
			}
			for _, n := range blue[i:min(i+batch, total)] {
				if err := u.DrainNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Draining node %s failed: %v", n, err)
				}
			}
		}
	}
	phase(containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING, "")
	d := DefaultNodePoolSoakDuration
	if bg.GetNodePoolSoakDuration() != nil {
		d = bg.GetNodePoolSoakDuration().AsDuration()
	}
	if waited+d > maxSoak {
		d = max(0, maxSoak-waited)
	}
	if err := sleep(ctx, d, soak, pause); err != nil {
		return err
	}
	if err := stop(); err != nil {
		return err
	}
	// "This phase can't be paused": CancelOperation refuses it.
	phase(containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL, s.stamp())
	for j, n := range blue {
		if err := u.RemoveNode(ctx, kind, n); err != nil {
			return status.Errorf(codes.Internal, "Deleting node %s failed: %v", n, err)
		}
		s.nodeProgress(ctx, pool, j+1, total, "Phase: DELETING_BLUE_POOL.")
	}
	return nil
}

// sleep waits d, or until early or pause is closed (a paused caller
// checks its stop next), or ctx is done.
func sleep(ctx context.Context, d time.Duration, early, pause chan struct{}) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-early:
	case <-pause:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// CompleteNodePoolUpgrade ends a blue-green upgrade's soak: the blue nodes
// are deleted now.
func (s *Server) CompleteNodePoolUpgrade(_ context.Context, req *containerpb.CompleteNodePoolUpgradeRequest) (*emptyResp, error) {
	p, err := parseName(req.GetName(), "", "", "", "", "", "pool")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, np, err := s.poolLocked(p)
	if err != nil {
		return nil, err
	}
	ch := e.soak[np.Name]
	if ch == nil || np.GetUpdateInfo().GetBlueGreenInfo().GetPhase() != containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING {
		return nil, status.Errorf(codes.FailedPrecondition, "Node pool %q is not soaking in a blue-green upgrade.", np.Name)
	}
	close(ch)
	delete(e.soak, np.Name)
	return &emptyResp{}, nil
}

// ---- maintenance policy ----

// Limits of a cluster's exclusions (re-read 2026-11-20:
// maintenance-windows-and-exclusions).
const (
	maxExclusions          = 20
	maxNoUpgradeExclusions = 3
)

// dailyWindowDuration is a daily maintenance window's: four hours.
const dailyWindowDuration = "PT4H0M0S"

// checkMaintenancePolicy refuses a policy GKE refuses and fills what GKE
// returns of it (a daily window's duration, the resource version).
func (s *Server) checkMaintenancePolicy(mp *containerpb.MaintenancePolicy) error {
	w := mp.GetWindow()
	if w == nil {
		return nil
	}
	if d := w.GetDailyMaintenanceWindow(); d != nil {
		if _, err := time.Parse("15:04", d.GetStartTime()); err != nil {
			return status.Errorf(codes.InvalidArgument, "daily_maintenance_window.start_time %q must be HH:MM in UTC.", d.GetStartTime())
		}
		d.Duration = dailyWindowDuration
	}
	if r := w.GetRecurringWindow(); r != nil {
		tw := r.GetWindow()
		if tw.GetStartTime() == nil || tw.GetEndTime() == nil || !tw.GetEndTime().AsTime().After(tw.GetStartTime().AsTime()) {
			return status.Error(codes.InvalidArgument, "recurring_window.window must have a start_time before its end_time.")
		}
		if _, err := parseRRule(r.GetRecurrence()); err != nil {
			return status.Errorf(codes.InvalidArgument, "recurring_window.recurrence %q: %v", r.GetRecurrence(), err)
		}
	}
	ex := w.GetMaintenanceExclusions()
	if len(ex) > maxExclusions {
		return status.Errorf(codes.InvalidArgument, "A cluster may have at most %d maintenance exclusions.", maxExclusions)
	}
	noUp := 0
	for name, tw := range ex {
		if tw.GetStartTime() == nil || tw.GetEndTime() == nil || !tw.GetEndTime().AsTime().After(tw.GetStartTime().AsTime()) {
			return status.Errorf(codes.InvalidArgument, "Maintenance exclusion %q must have a start_time before its end_time.", name)
		}
		if tw.GetMaintenanceExclusionOptions().GetScope() == containerpb.MaintenanceExclusionOptions_NO_UPGRADES {
			noUp++
		}
	}
	if noUp > maxNoUpgradeExclusions {
		return status.Errorf(codes.InvalidArgument, "A cluster may have at most %d \"no upgrades\" maintenance exclusions.", maxNoUpgradeExclusions)
	}
	return nil
}

// SetMaintenancePolicy replaces a cluster's maintenance window and
// exclusions, given the resource version of the policy it has.
func (s *Server) SetMaintenancePolicy(_ context.Context, req *containerpb.SetMaintenancePolicyRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	mp := proto.Clone(req.GetMaintenancePolicy()).(*containerpb.MaintenancePolicy)
	if mp == nil {
		mp = &containerpb.MaintenancePolicy{}
	}
	if err := s.checkMaintenancePolicy(mp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	cur := e.cluster.GetMaintenancePolicy().GetResourceVersion()
	if rv := mp.GetResourceVersion(); rv != "" && rv != cur {
		// UNVERIFIED wording (re-read 2026-11-20: the API reference's
		// MaintenancePolicy); the resource version check is documented on
		// the field.
		return nil, status.Error(codes.FailedPrecondition, "The maintenance policy was changed since it was read: its resource_version does not match.")
	}
	mp.ResourceVersion = shortHash(cur + s.stamp())
	e.cluster.MaintenancePolicy = mp
	return s.doneOpLocked(p, containerpb.Operation_UPDATE_CLUSTER, e.cluster.SelfLink), nil
}

// rrule is the part of an RFC 5545 recurrence the API's windows use.
type rrule struct {
	freq  string
	byDay map[time.Weekday]bool
}

var rruleDays = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

func parseRRule(s string) (rrule, error) {
	r := rrule{byDay: map[time.Weekday]bool{}}
	for _, part := range strings.Split(strings.TrimPrefix(s, "RRULE:"), ";") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "FREQ":
			if v != "DAILY" && v != "WEEKLY" {
				return r, fmt.Errorf("FREQ=%s is not available locally (DAILY or WEEKLY)", v)
			}
			r.freq = v
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				wd, ok := rruleDays[d]
				if !ok {
					return r, fmt.Errorf("BYDAY %q is not a day", d)
				}
				r.byDay[wd] = true
			}
		case "INTERVAL", "WKST":
		default:
			return r, fmt.Errorf("%s is not available locally", k)
		}
	}
	if r.freq == "" {
		return r, fmt.Errorf("FREQ is required")
	}
	return r, nil
}

// inWindow is whether now is inside the maintenance window; with no window
// every time is.
func inWindow(mp *containerpb.MaintenancePolicy, now time.Time) bool {
	w := mp.GetWindow()
	now = now.UTC()
	if d := w.GetDailyMaintenanceWindow(); d != nil {
		st, err := time.Parse("15:04", d.GetStartTime())
		if err != nil {
			return false
		}
		for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
			start := time.Date(day.Year(), day.Month(), day.Day(), st.Hour(), st.Minute(), 0, 0, time.UTC)
			if !now.Before(start) && now.Before(start.Add(4*time.Hour)) {
				return true
			}
		}
		return false
	}
	if r := w.GetRecurringWindow(); r != nil {
		rr, err := parseRRule(r.GetRecurrence())
		if err != nil {
			return false
		}
		start, end := r.GetWindow().GetStartTime().AsTime(), r.GetWindow().GetEndTime().AsTime()
		dur := end.Sub(start)
		if now.Before(start) {
			return false
		}
		// The occurrences that could cover now started at most dur ago.
		for d := 0; d <= int(dur/(24*time.Hour))+1; d++ {
			day := now.AddDate(0, 0, -d)
			occ := time.Date(day.Year(), day.Month(), day.Day(), start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
			if occ.Before(start) {
				continue
			}
			if rr.freq == "WEEKLY" && len(rr.byDay) > 0 && !rr.byDay[occ.Weekday()] {
				continue
			}
			if rr.freq == "WEEKLY" && len(rr.byDay) == 0 && occ.Weekday() != start.Weekday() {
				continue
			}
			if !now.Before(occ) && now.Before(occ.Add(dur)) {
				return true
			}
		}
		return false
	}
	return true
}

// excluded is whether an exclusion covering now blocks an automatic
// upgrade: of the control plane or of nodes, minor or patch.
func excluded(mp *containerpb.MaintenancePolicy, now time.Time, node, minor bool) bool {
	for _, tw := range mp.GetWindow().GetMaintenanceExclusions() {
		if now.Before(tw.GetStartTime().AsTime()) || !now.Before(tw.GetEndTime().AsTime()) {
			continue
		}
		switch tw.GetMaintenanceExclusionOptions().GetScope() {
		case containerpb.MaintenanceExclusionOptions_NO_UPGRADES:
			return true
		case containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES:
			if minor {
				return true
			}
		case containerpb.MaintenanceExclusionOptions_NO_MINOR_OR_NODE_UPGRADES:
			if minor || node {
				return true
			}
		}
	}
	return false
}

// AutoUpgradeAllowed is whether an automatic upgrade may start now under
// the cluster's maintenance policy.
func AutoUpgradeAllowed(mp *containerpb.MaintenancePolicy, now time.Time, node, minor bool) bool {
	return inWindow(mp, now) && !excluded(mp, now, node, minor)
}

// ---- automatic upgrades ----

// channelTarget is the version a cluster's release channel upgrades it
// to: the default, reached one minor version at a time.
func (s *Server) channelTarget(from string) (Version, bool) {
	def := s.opts.Versions[0]
	if compareVersions(def.GKE, from) <= 0 {
		return Version{}, false
	}
	var next Version
	for _, v := range s.opts.Versions {
		if compareVersions(v.GKE, from) > 0 && minorOf(v.GKE) <= minorOf(from)+1 &&
			(next.GKE == "" || compareVersions(v.GKE, next.GKE) > 0) {
			next = v
		}
	}
	return next, next.GKE != ""
}

// autoUpgradeLoop starts the automatic upgrades every interval.
func (s *Server) autoUpgradeLoop(every time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-t.C:
			s.AutoUpgrade()
		}
	}
}

// AutoUpgrade starts, for each cluster that needs one and whose
// maintenance policy allows it now, its control plane's upgrade to its
// channel's target, or else a node pool's (with auto-upgrade on) to the
// control plane's version. It returns the operations started.
func (s *Server) AutoUpgrade() []*containerpb.Operation {
	u, err := s.upgrader()
	if err != nil {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var ops []*containerpb.Operation
	for _, e := range s.clusters {
		c := e.cluster
		if e.busy != "" || c.Status != containerpb.Cluster_RUNNING {
			continue
		}
		p := parsed{project: e.project, location: c.Location, cluster: c.Name}
		mp := c.GetMaintenancePolicy()
		if to, ok := s.channelTarget(c.CurrentMasterVersion); ok {
			if checkMasterUpgrade(c, to) == nil && AutoUpgradeAllowed(mp, now, false, minorDiffers(to.GKE, c.CurrentMasterVersion)) {
				ops = append(ops, s.startMasterUpgradeLocked(p, e, to, u))
			}
			continue
		}
		for _, np := range c.NodePools {
			if !np.GetManagement().GetAutoUpgrade() || compareVersions(np.Version, c.CurrentMasterVersion) >= 0 {
				continue
			}
			to, err := s.resolveVersion("Node version", c.CurrentMasterVersion)
			if err != nil || !AutoUpgradeAllowed(mp, now, true, minorDiffers(to.GKE, np.Version)) {
				continue
			}
			pp := p
			pp.pool = np.Name
			ops = append(ops, s.startPoolUpgradeLocked(pp, e, np, to, u, true))
			break
		}
	}
	return ops
}
