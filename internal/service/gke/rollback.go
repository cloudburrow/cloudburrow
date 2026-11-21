package gke

import (
	"context"
	"strconv"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Rolling back a node pool upgrade (#2076), as GKE does it (re-read
// 2026-11-20:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/upgrading-a-cluster#rollback,
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pool-upgrade-strategies,
// https://docs.cloud.google.com/sdk/gcloud/reference/container/node-pools/rollback):
//
//   - "Rollback is a method used after a canceled or failed node-pool
//     upgrade", or one "incomplete due to a maintenance window timing
//     out": the nodes already upgraded are recreated on the pool's
//     version ("Rolling back an incomplete surge upgrade" is an instance
//     re-creation); "Nodes that have not started an upgrade are
//     unaffected", and "You cannot roll back node pools once they have
//     been successfully upgraded";
//   - a blue-green upgrade can be rolled back while it runs, in every
//     phase before DELETING_BLUE_POOL ("This phase can't be paused"): the
//     upgrade stops, the blue nodes are uncordoned "so that workloads may
//     be rescheduled on it", and the green nodes are drained and deleted;
//     the pool's phase is ROLLBACK_STARTED meanwhile;
//   - respect_pdb: "Option for rollback to ignore the PodDisruptionBudget.
//     Default value is false" (the API reference); with it, the drains use
//     the eviction API, as an upgrade's do.
//
// The rollback is an UPGRADE_NODES operation ("most forms of updates to
// node pools", the Operation.Type reference), which RUNNING then DONE.

// rollbackPhases are the blue-green phases a running upgrade can be rolled
// back in.
var rollbackPhases = map[containerpb.NodePool_UpdateInfo_BlueGreenInfo_Phase]bool{
	containerpb.NodePool_UpdateInfo_BlueGreenInfo_UPDATE_STARTED:      true,
	containerpb.NodePool_UpdateInfo_BlueGreenInfo_CREATING_GREEN_POOL: true,
	containerpb.NodePool_UpdateInfo_BlueGreenInfo_CORDONING_BLUE_POOL: true,
	containerpb.NodePool_UpdateInfo_BlueGreenInfo_DRAINING_BLUE_POOL:  true,
	containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING:   true,
}

// RollbackNodePoolUpgrade rolls back a cancelled, failed, paused or (blue-
// green) running node pool upgrade.
func (s *Server) RollbackNodePoolUpgrade(ctx context.Context, req *containerpb.RollbackNodePoolUpgradeRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	u, err := s.upgrader()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	e, np, err := s.poolLocked(p)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	running, err := s.rollbackRunningLocked(p, e, np)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	busy, kind, version := e.busy, e.kind, np.Version
	s.mu.Unlock()

	if running == nil {
		// Only a partly upgraded pool has something to roll back.
		nodes, err := u.PoolNodes(ctx, kind, np.GetName())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Listing node pool %q's nodes failed: %v", np.GetName(), err)
		}
		img := s.NodeImage(version)
		upgraded := 0
		for _, n := range nodes {
			if n.Image != img {
				upgraded++
			}
		}
		if upgraded == 0 {
			// UNVERIFIED wording (re-read 2026-11-20: concepts/node-pool-upgrade-strategies).
			return nil, status.Errorf(codes.FailedPrecondition,
				"Node pool %q has no incomplete upgrade to roll back: every node runs %s. A node pool cannot be rolled back once it has been successfully upgraded; downgrade it instead.",
				np.GetName(), version)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if e.busy != busy || np.Version != version {
		return nil, busyErr(e)
	}
	if running != nil {
		running.stopGracefully()
		running.op.Status = containerpb.Operation_ABORTING
	}
	return s.startRollbackLocked(p, e, np, u, running, req.GetRespectPdb()), nil
}

// rollbackRunningLocked is the running blue-green upgrade of np that a
// rollback stops, nil when none runs; it refuses what cannot be rolled
// back now.
func (s *Server) rollbackRunningLocked(p parsed, e *entry, np *containerpb.NodePool) (*opEntry, error) {
	if e.busy == "" {
		if e.cluster.Status != containerpb.Cluster_RUNNING && e.cluster.Status != containerpb.Cluster_DEGRADED {
			return nil, busyErr(e)
		}
		return nil, nil
	}
	o := s.ops[opName(p.project, p.location, e.busy)]
	if o == nil || o.pool != np || o.op.Status == containerpb.Operation_ABORTING {
		return nil, busyErr(e)
	}
	ph := np.GetUpdateInfo().GetBlueGreenInfo().GetPhase()
	switch {
	case ph == containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL:
		// UNVERIFIED wording (re-read 2026-11-20: concepts/node-pool-upgrade-strategies).
		return nil, status.Errorf(codes.FailedPrecondition, "Node pool %q is deleting its blue nodes: a blue-green upgrade cannot be rolled back in the DELETING_BLUE_POOL phase.", np.Name)
	case rollbackPhases[ph]:
		return o, nil
	}
	// A surge upgrade is rolled back after it is cancelled or fails.
	// UNVERIFIED wording (re-read 2026-11-20: concepts/node-pool-upgrade-strategies).
	return nil, status.Errorf(codes.FailedPrecondition, "Node pool %q is being upgraded (operation %s): cancel the upgrade before rolling it back.", np.Name, e.busy)
}

// startRollbackLocked starts np's rollback: after running (if any) has
// stopped, the nodes not on np's version are replaced by nodes of it.
func (s *Server) startRollbackLocked(p parsed, e *entry, np *containerpb.NodePool, u Upgrader, running *opEntry, respectPDB bool) *containerpb.Operation {
	c := e.cluster
	kind, pool := e.kind, np.Name
	blueGreen := np.GetUpdateInfo().GetBlueGreenInfo() != nil ||
		np.GetUpgradeSettings().GetStrategy() == containerpb.NodePoolUpdateStrategy_BLUE_GREEN
	bgInfo := func() {
		if !blueGreen {
			return
		}
		if np.GetUpdateInfo().GetBlueGreenInfo() == nil {
			np.UpdateInfo = &containerpb.NodePool_UpdateInfo{BlueGreenInfo: &containerpb.NodePool_UpdateInfo_BlueGreenInfo{}}
		}
		np.UpdateInfo.BlueGreenInfo.Phase = containerpb.NodePool_UpdateInfo_BlueGreenInfo_ROLLBACK_STARTED
	}
	c.Status = containerpb.Cluster_RECONCILING
	np.Status = containerpb.NodePool_RECONCILING
	bgInfo()
	drain := u.ForceDrainNode
	if respectPDB {
		drain = u.DrainNode
	}
	return s.startOpLocked(p, containerpb.Operation_UPGRADE_NODES, np.SelfLink, e, func(ctx context.Context) error {
		if running != nil {
			select {
			case <-running.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			// The stopped upgrade's end set the pool RUNNING: it is the
			// rollback's again.
			s.mu.Lock()
			c.Status = containerpb.Cluster_RECONCILING
			np.Status = containerpb.NodePool_RECONCILING
			bgInfo()
			s.mu.Unlock()
		}
		s.mu.Lock()
		img := s.NodeImage(np.Version)
		s.mu.Unlock()
		nodes, err := u.PoolNodes(ctx, kind, pool)
		if err != nil {
			return status.Errorf(codes.Internal, "Listing node pool %q's nodes failed: %v", pool, err)
		}
		var keep, back []string
		for _, n := range nodes {
			if n.Image == img {
				keep = append(keep, n.Name)
			} else {
				back = append(back, n.Name)
			}
		}
		total := len(back)
		progress := func(done int, stage string) {
			s.progress(ctx, func(op *containerpb.Operation) {
				op.Detail = "Rolling back " + pool + ": " + strconv.Itoa(done) + " of " + strconv.Itoa(total) + " nodes rolled back. " + stage
				op.Progress = &containerpb.OperationProgress{Name: pool, Status: containerpb.Operation_RUNNING,
					Metrics: []*containerpb.OperationProgress_Metric{intMetric("NODES_TOTAL", total), intMetric("NODES_DONE", done)}}
			})
		}
		progress(0, "")
		// The nodes kept (a blue-green upgrade's blue pool, cordoned or
		// drained) take workloads again.
		for _, n := range keep {
			if err := u.UncordonNode(ctx, kind, n); err != nil {
				return status.Errorf(codes.Internal, "Uncordoning node %s failed: %v", n, err)
			}
		}
		if blueGreen {
			// The green pool is drained and deleted.
			for _, n := range back {
				if err := u.CordonNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Cordoning node %s failed: %v", n, err)
				}
			}
			for i, n := range back {
				progress(i, "Draining "+n+".")
				if err := drain(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Draining node %s failed: %v", n, err)
				}
				if err := u.RemoveNode(ctx, kind, n); err != nil {
					return status.Errorf(codes.Internal, "Deleting node %s failed: %v", n, err)
				}
			}
			progress(total, "")
			return nil
		}
		// A surge upgrade's upgraded nodes are recreated on the pool's
		// version, one at a time: the new node is Ready before the
		// upgraded one is drained.
		for i, n := range back {
			if _, err := u.AddNode(ctx, kind, img, pool); err != nil {
				return status.Errorf(codes.Internal, "Recreating a node of %q failed: %v", pool, err)
			}
			if err := u.CordonNode(ctx, kind, n); err != nil {
				return status.Errorf(codes.Internal, "Cordoning node %s failed: %v", n, err)
			}
			progress(i, "Draining "+n+".")
			if err := drain(ctx, kind, n); err != nil {
				return status.Errorf(codes.Internal, "Draining node %s failed: %v", n, err)
			}
			if err := u.RemoveNode(ctx, kind, n); err != nil {
				return status.Errorf(codes.Internal, "Deleting node %s failed: %v", n, err)
			}
			progress(i+1, "")
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		np.UpdateInfo = nil
		if err != nil {
			np.Status = containerpb.NodePool_RUNNING_WITH_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		np.Status = containerpb.NodePool_RUNNING
		np.StatusMessage = ""
		c.CurrentNodeVersion = oldestNodeVersion(c)
	})
}
