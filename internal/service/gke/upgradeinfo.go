package gke

import (
	"context"
	"strconv"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Upgrade information and rollback-safe control plane upgrades (#2100),
// as Google documents them (re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters/fetchClusterUpgradeInfo,
// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/ClusterUpgradeInfo,
// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/NodePoolUpgradeInfo,
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster#two-step-cp-upgrade):
//   - FetchClusterUpgradeInfo and FetchNodePoolUpgradeInfo are the targets
//     automatic upgrades would move to (the release channel's, from the
//     versions served), whether they are paused now and why (the
//     maintenance window and exclusions the auto-upgrade loop obeys), and
//     the upgrades the cluster or pool has had;
//   - a minor control plane upgrade with desired_rollback_safe_upgrade is
//     done in two steps: step one moves the binaries and keeps the previous
//     minor version's behaviour (the API server, controller manager and
//     scheduler run with --emulated-version, Kubernetes' compatibility
//     versions); CompleteControlPlaneUpgrade, or the end of the soak, is
//     step two, which drops the emulation. Until step two the control
//     plane can go back to its previous version ("The VERSION must be the
//     exact patch version previously used", the rollbackSafeUpgradeStatus'
//     previousVersion): the kept previous binaries and manifests run
//     again, the data being the previous minor's; no other upgrade of the
//     control plane runs meanwhile, and nodes are upgraded no further than
//     the emulated version;
//   - the end of standard and of extended support of the version's minor
//     are GKE's release schedule's (schedule.go).

// clusterExtra is what is kept of a cluster beside its record.
type clusterExtra struct {
	Upgrades []upgradeRecord `json:"upgrades,omitempty"`
	// RollbackSafe is a rollback-safe upgrade between its two steps.
	RollbackSafe *rollbackSafeState `json:"rollbackSafe,omitempty"`
	// Rotation is a credential or IP rotation in progress (rotation.go).
	Rotation *rotationState `json:"rotation,omitempty"`
}

// upgradeRecord is one upgrade of the control plane (Pool "") or a pool.
type upgradeRecord struct {
	Op    string    `json:"op"`
	Pool  string    `json:"pool,omitempty"`
	From  string    `json:"from"`
	To    string    `json:"to"`
	Auto  bool      `json:"auto,omitempty"`
	Start time.Time `json:"start"`
	// FromEmulated and ToEmulated are a rollback-safe upgrade's.
	FromEmulated string `json:"fromEmulated,omitempty"`
	ToEmulated   string `json:"toEmulated,omitempty"`
}

// rollbackSafeState is a control plane between step one and step two.
type rollbackSafeState struct {
	Previous string    `json:"previous"`
	End      time.Time `json:"end"`
}

// Emulator is what a Backend does for rollback-safe upgrades.
type Emulator interface {
	// SetEmulatedVersion runs the control plane's components with
	// --emulated-version=minor ("1.35"), or without it when minor is "".
	SetEmulatedVersion(ctx context.Context, kind, minor string) error
}

// Rollback-safe soak limits: six hours to seven days, a day by default
// (the default of automatic upgrades; UNVERIFIED for manual ones).
// Docs re-read 2026-11-21: nothing more is published.
const (
	minRollbackSoak     = 6 * time.Hour
	maxRollbackSoak     = 7 * 24 * time.Hour
	defaultRollbackSoak = 24 * time.Hour
)

// minorVersion is "X.Y" of a version.
func minorVersion(v string) string {
	p := versionParts(v)
	return strconv.Itoa(p[0]) + "." + strconv.Itoa(p[1])
}

// recordUpgradeLocked notes an upgrade the operation op runs.
func (s *Server) recordUpgradeLocked(e *entry, op *containerpb.Operation, pool, from, to string, auto bool) *upgradeRecord {
	e.extra.Upgrades = append(e.extra.Upgrades, upgradeRecord{Op: op.Name, Pool: pool, From: from, To: to, Auto: auto, Start: s.now()})
	return &e.extra.Upgrades[len(e.extra.Upgrades)-1]
}

// upgradeDetails are the records of pool ("" the control plane).
func (s *Server) upgradeDetailsLocked(p parsed, e *entry, pool string) []*containerpb.UpgradeDetails {
	var out []*containerpb.UpgradeDetails
	for _, r := range e.extra.Upgrades {
		if r.Pool != pool {
			continue
		}
		d := &containerpb.UpgradeDetails{InitialVersion: r.From, TargetVersion: r.To, StartTime: timestamppb.New(r.Start),
			State: containerpb.UpgradeDetails_RUNNING, StartType: containerpb.UpgradeDetails_MANUAL,
			InitialEmulatedVersion: r.FromEmulated, TargetEmulatedVersion: r.ToEmulated}
		if r.Auto {
			d.StartType = containerpb.UpgradeDetails_AUTOMATIC
		}
		if oe := s.ops[opName(p.project, p.location, r.Op)]; oe != nil && oe.op.Status == containerpb.Operation_DONE {
			switch {
			case oe.op.GetError() != nil && oe.op.GetError().GetCode() == int32(codes.Canceled):
				d.State = containerpb.UpgradeDetails_CANCELED
			case oe.op.GetError() != nil:
				d.State = containerpb.UpgradeDetails_FAILED
			default:
				d.State = containerpb.UpgradeDetails_SUCCEEDED
			}
			if t, err := time.Parse(time.RFC3339Nano, oe.op.EndTime); err == nil {
				d.EndTime = timestamppb.New(t)
			}
		}
		out = append(out, d)
	}
	return out
}

// targets are the minor and patch versions automatic upgrades move from
// to: the newest served version of the next minor, and of the same minor.
func (s *Server) targets(from string) (minor, patch string) {
	for _, v := range s.opts.Versions {
		switch {
		case compareVersions(v.GKE, from) <= 0:
		case minorOf(v.GKE) == minorOf(from):
			if patch == "" || compareVersions(v.GKE, patch) > 0 {
				patch = v.GKE
			}
		case minorOf(v.GKE) == minorOf(from)+1:
			if minor == "" || compareVersions(v.GKE, minor) > 0 {
				minor = v.GKE
			}
		}
	}
	return minor, patch
}

// pauses is why automatic upgrades are paused now, for nodes or the
// control plane, and whether minor (paused) and all upgrades (paused) are.
func pauses(mp *containerpb.MaintenancePolicy, now time.Time, node bool) (reasons []containerpb.ClusterUpgradeInfo_AutoUpgradePausedReason, minorPaused, allPaused bool) {
	if !inWindow(mp, now) {
		reasons = append(reasons, containerpb.ClusterUpgradeInfo_MAINTENANCE_WINDOW)
		allPaused = true
	}
	for _, tw := range mp.GetWindow().GetMaintenanceExclusions() {
		if now.Before(tw.GetStartTime().AsTime()) || !now.Before(tw.GetEndTime().AsTime()) {
			continue
		}
		switch tw.GetMaintenanceExclusionOptions().GetScope() {
		case containerpb.MaintenanceExclusionOptions_NO_UPGRADES:
			reasons = append(reasons, containerpb.ClusterUpgradeInfo_MAINTENANCE_EXCLUSION_NO_UPGRADES)
			allPaused = true
		case containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES:
			reasons = append(reasons, containerpb.ClusterUpgradeInfo_MAINTENANCE_EXCLUSION_NO_MINOR_UPGRADES)
			minorPaused = true
		case containerpb.MaintenanceExclusionOptions_NO_MINOR_OR_NODE_UPGRADES:
			reasons = append(reasons, containerpb.ClusterUpgradeInfo_MAINTENANCE_EXCLUSION_NO_MINOR_UPGRADES)
			minorPaused = true
			if node {
				allPaused = true
			}
		}
	}
	return reasons, minorPaused, allPaused
}

func upgradeStatus(minorPaused, allPaused bool) containerpb.ClusterUpgradeInfo_AutoUpgradeStatus {
	switch {
	case allPaused:
		return containerpb.ClusterUpgradeInfo_UPGRADE_PAUSED
	case minorPaused:
		return containerpb.ClusterUpgradeInfo_MINOR_UPGRADE_PAUSED
	}
	return containerpb.ClusterUpgradeInfo_ACTIVE
}

func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// FetchClusterUpgradeInfo is the control plane's upgrade information.
func (s *Server) FetchClusterUpgradeInfo(_ context.Context, req *containerpb.FetchClusterUpgradeInfoRequest) (*containerpb.ClusterUpgradeInfo, error) {
	p, err := parseName(req.GetName(), "", "", "", "", "", "cluster")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	c := e.cluster
	minor, patch := s.targets(c.CurrentMasterVersion)
	reasons, minorPaused, allPaused := pauses(c.GetMaintenancePolicy(), s.now(), false)
	info := &containerpb.ClusterUpgradeInfo{
		MinorTargetVersion: optString(minor),
		PatchTargetVersion: optString(patch),
		AutoUpgradeStatus:  []containerpb.ClusterUpgradeInfo_AutoUpgradeStatus{upgradeStatus(minorPaused, allPaused)},
		PausedReason:       reasons,
		UpgradeDetails:     s.upgradeDetailsLocked(p, e, ""),
	}
	std, ext := supportEnds(c.CurrentMasterVersion)
	info.EndOfStandardSupportTimestamp, info.EndOfExtendedSupportTimestamp = optString(std), optString(ext)
	if rs := e.extra.RollbackSafe; rs != nil {
		info.RollbackSafeUpgradeStatus = &containerpb.RollbackSafeUpgradeStatus{
			Mode:                               containerpb.RollbackSafeUpgradeStatus_KCP_MINOR_UPGRADE_ROLLBACK_SAFE_MODE,
			ControlPlaneUpgradeRollbackEndTime: timestamppb.New(rs.End),
			PreviousVersion:                    rs.Previous,
		}
	}
	return info, nil
}

// FetchNodePoolUpgradeInfo is a node pool's upgrade information: its
// target is the control plane's version, which nodes are never newer than.
func (s *Server) FetchNodePoolUpgradeInfo(_ context.Context, req *containerpb.FetchNodePoolUpgradeInfoRequest) (*containerpb.NodePoolUpgradeInfo, error) {
	p, err := parseName(req.GetName(), "", "", "", "", "", "pool")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	np := findPool(e.cluster, p.pool)
	if np == nil {
		return nil, notFound(req.GetName())
	}
	info := &containerpb.NodePoolUpgradeInfo{UpgradeDetails: s.upgradeDetailsLocked(p, e, np.Name)}
	std, ext := supportEnds(np.Version)
	info.EndOfStandardSupportTimestamp, info.EndOfExtendedSupportTimestamp = optString(std), optString(ext)
	if compareVersions(e.cluster.CurrentMasterVersion, np.Version) > 0 {
		if minorDiffers(e.cluster.CurrentMasterVersion, np.Version) {
			info.MinorTargetVersion = optString(e.cluster.CurrentMasterVersion)
		} else {
			info.PatchTargetVersion = optString(e.cluster.CurrentMasterVersion)
		}
	}
	if !np.GetManagement().GetAutoUpgrade() {
		// Auto-upgrade off: nothing moves the pool but the user.
		info.AutoUpgradeStatus = []containerpb.NodePoolUpgradeInfo_AutoUpgradeStatus{containerpb.NodePoolUpgradeInfo_UPGRADE_PAUSED}
		return info, nil
	}
	reasons, minorPaused, allPaused := pauses(e.cluster.GetMaintenancePolicy(), s.now(), true)
	// The two messages number their enums differently: by name.
	info.AutoUpgradeStatus = []containerpb.NodePoolUpgradeInfo_AutoUpgradeStatus{containerpb.NodePoolUpgradeInfo_AutoUpgradeStatus(
		containerpb.NodePoolUpgradeInfo_AutoUpgradeStatus_value[upgradeStatus(minorPaused, allPaused).String()])}
	for _, r := range reasons {
		info.PausedReason = append(info.PausedReason, containerpb.NodePoolUpgradeInfo_AutoUpgradePausedReason(
			containerpb.NodePoolUpgradeInfo_AutoUpgradePausedReason_value[r.String()]))
	}
	return info, nil
}

// checkRollbackSafe refuses a rollback-safe upgrade GKE refuses: it is a
// minor upgrade of the control plane, its soak six hours to seven days.
func checkRollbackSafe(c *containerpb.Cluster, to Version, rs *containerpb.RollbackSafeUpgrade) (time.Duration, error) {
	if !minorDiffers(to.GKE, c.CurrentMasterVersion) {
		// UNVERIFIED wording; documented for minor upgrades only.
		// Docs re-read 2026-11-21: nothing more is published.
		return 0, status.Error(codes.InvalidArgument, "A rollback-safe upgrade is a minor version upgrade of the control plane.")
	}
	d := defaultRollbackSoak
	if rs.GetControlPlaneSoakDuration() != nil {
		d = rs.GetControlPlaneSoakDuration().AsDuration()
	}
	if d < minRollbackSoak || d > maxRollbackSoak {
		return 0, status.Errorf(codes.InvalidArgument, "control_plane_soak_duration must be between %s and %s.", minRollbackSoak, maxRollbackSoak)
	}
	return d, nil
}

// CompleteControlPlaneUpgrade is a rollback-safe upgrade's step two: the
// control plane leaves the previous minor version's emulation, after
// which it can no longer go back to it.
func (s *Server) CompleteControlPlaneUpgrade(_ context.Context, req *containerpb.CompleteControlPlaneUpgradeRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), "", "", "", "", "", "cluster")
	if err != nil {
		return nil, err
	}
	em, ok := s.backend.(Emulator)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Rollback-safe upgrades are not available with this backend.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if e.extra.RollbackSafe == nil {
		// UNVERIFIED wording; the precondition is documented.
		// Docs re-read 2026-11-21: nothing more is published.
		return nil, status.Error(codes.FailedPrecondition, "The cluster has no rollback-safe control plane upgrade to complete.")
	}
	return s.completeRollbackSafeLocked(p, e, em, false), nil
}

// completeRollbackSafeLocked runs step two.
func (s *Server) completeRollbackSafeLocked(p parsed, e *entry, em Emulator, auto bool) *containerpb.Operation {
	c := e.cluster
	c.Status = containerpb.Cluster_RECONCILING
	kind := e.kind
	from := c.CurrentEmulatedVersion
	to := c.CurrentMasterVersion
	op := s.startOpLocked(p, containerpb.Operation_UPGRADE_MASTER, c.SelfLink, e, func(ctx context.Context) error {
		if err := em.SetEmulatedVersion(ctx, kind, ""); err != nil {
			return status.Errorf(codes.Internal, "Completing the control plane upgrade failed: %v", err)
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		c.CurrentEmulatedVersion = to
		e.extra.RollbackSafe = nil
	})
	r := s.recordUpgradeLocked(e, op, "", to, to, auto)
	r.FromEmulated, r.ToEmulated = from, to
	return op
}

// completeExpiredRollbacks runs step two of every rollback-safe upgrade
// whose soak has ended, as GKE does; s.mu is held.
func (s *Server) completeExpiredRollbacksLocked(now time.Time) []*containerpb.Operation {
	em, ok := s.backend.(Emulator)
	if !ok {
		return nil
	}
	var ops []*containerpb.Operation
	for _, e := range s.clusters {
		rs := e.extra.RollbackSafe
		if rs == nil || now.Before(rs.End) || e.busy != "" || e.cluster.Status != containerpb.Cluster_RUNNING {
			continue
		}
		p := parsed{project: e.project, location: e.cluster.Location, cluster: e.cluster.Name}
		ops = append(ops, s.completeRollbackSafeLocked(p, e, em, true))
	}
	return ops
}

// ControlPlaneRollbacker is what a Backend does to take a control plane
// back to the version it ran before its last upgrade.
type ControlPlaneRollbacker interface {
	// RollbackControlPlane runs the control plane on image's version, the
	// one it ran before its last upgrade, again.
	RollbackControlPlane(ctx context.Context, kind, image string) error
}

// rollbackControlPlaneLocked answers an upgrade of the control plane
// during a rollback-safe upgrade's soak: to the previous version it is the
// rollback, an UPGRADE_MASTER operation; to any other it is refused.
func (s *Server) rollbackControlPlaneLocked(p parsed, e *entry, to Version) (*containerpb.Operation, error) {
	c := e.cluster
	rs := e.extra.RollbackSafe
	if to.GKE != rs.Previous {
		if compareVersions(to.GKE, c.CurrentMasterVersion) < 0 {
			// Documented rule; UNVERIFIED wording (re-read 2026-11-22:
			// how-to/upgrading-a-cluster, "The VERSION must be the exact
			// patch version previously used").
			return nil, status.Errorf(codes.InvalidArgument,
				"Master version %q: a rollback-safe upgrade rolls back to the exact patch version previously used, %s.", to.GKE, rs.Previous)
		}
		// UNVERIFIED wording, re-read 2026-11-22: how-to/upgrading-a-cluster;
		// documented: "GKE doesn't perform any type of
		// automatic upgrades to the control plane or nodes" until step two.
		return nil, status.Errorf(codes.FailedPrecondition,
			"The control plane is in a rollback-safe upgrade's soak until %s: complete the upgrade (CompleteControlPlaneUpgrade) or roll back to %s first.",
			rs.End.UTC().Format(time.RFC3339), rs.Previous)
	}
	rb, ok := s.backend.(ControlPlaneRollbacker)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Rollback-safe upgrades are not available with this backend.")
	}
	for _, np := range c.GetNodePools() {
		if compareVersions(np.GetVersion(), to.GKE) > 0 {
			// Nodes are never newer than the emulated version, so this
			// holds unless a pool was moved past it some other way.
			return nil, status.Errorf(codes.FailedPrecondition, "Node pool %q runs %s, newer than %s.", np.GetName(), np.GetVersion(), to.GKE)
		}
	}
	c.Status = containerpb.Cluster_RECONCILING
	kind := e.kind
	abac := c.GetLegacyAbac().GetEnabled()
	from := c.CurrentMasterVersion
	op := s.startOpLocked(p, containerpb.Operation_UPGRADE_MASTER, c.SelfLink, e, func(ctx context.Context) error {
		s.progress(ctx, func(op *containerpb.Operation) {
			op.Detail = "Rolling the control plane back to " + to.GKE + "."
			op.Progress = &containerpb.OperationProgress{Name: "control-plane", Status: containerpb.Operation_RUNNING}
		})
		if err := rb.RollbackControlPlane(ctx, kind, to.Image); err != nil {
			return status.Errorf(codes.Internal, "Rolling the control plane back to %s failed: %v", to.GKE, err)
		}
		if abac {
			if ctl, ok := s.backend.(Controller); ok {
				if err := ctl.SetLegacyAbac(ctx, kind, true); err != nil {
					return status.Errorf(codes.Internal, "Enabling legacy ABAC after the rollback failed: %v", err)
				}
			}
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
		c.CurrentEmulatedVersion = to.GKE
		e.extra.RollbackSafe = nil
	})
	r := s.recordUpgradeLocked(e, op, "", from, to.GKE, false)
	r.FromEmulated, r.ToEmulated = rs.Previous, to.GKE
	return op, nil
}
