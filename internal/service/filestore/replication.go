package filestore

import (
	"context"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Replication (#2192), as the v1 reference describes it (read
// 2026-11-23): a replica is made as a new instance whose
// replication.role is STANDBY ("When creating a new replica, this field
// must be set to STANDBY") and whose one replica's peer_instance names the
// active instance ("required when creating a replica"; "Only a single
// replica is supported"). Each side then shows the other as its replica's
// peer_instance, READY, with last_active_sync_time "the timestamp of the
// latest replication snapshot taken on the active instance and is already
// replicated safely". PromoteReplica promotes the standby, called on it or
// on the active instance with peer_instance naming it; the two are then
// independent instances, the promoted one with the data of the last
// replicated snapshot.
//
// Replication is offered by the Regional and Enterprise tiers, and the
// replica has its active instance's tier, protocol and capacity (the
// reference does not say; UNVERIFIED, re-read 2026-11-23). The standby's share is read-only
// to its clients. Its data is a copy of the active share's, taken every
// ReplicationEvery.

func replicable(t filestorepb.Instance_Tier) bool {
	return t == filestorepb.Instance_REGIONAL || t == filestorepb.Instance_ENTERPRISE
}

// checkReplicaLocked checks a new instance's replication and answers its
// active instance's record (nil when it is not a replica).
func (s *Server) checkReplicaLocked(in *filestorepb.Instance) (*instRecord, error) {
	rp := in.GetReplication()
	if rp == nil || (rp.GetRole() == filestorepb.Replication_ROLE_UNSPECIFIED && len(rp.GetReplicas()) == 0) {
		in.Replication = nil
		return nil, nil
	}
	if rp.GetRole() != filestorepb.Replication_STANDBY {
		return nil, apierror.InvalidArgument("replication.role must be STANDBY when creating a replica")
	}
	if len(rp.GetReplicas()) != 1 {
		return nil, apierror.InvalidArgument("replication.replicas must have exactly one entry: only a single replica is supported")
	}
	peer := rp.GetReplicas()[0].GetPeerInstance()
	if peer == "" {
		return nil, apierror.InvalidArgument("replication.replicas[0].peer_instance is required when creating a replica")
	}
	if !replicable(in.GetTier()) {
		return nil, apierror.InvalidArgument("Replication is not supported for tier %s: only REGIONAL and ENTERPRISE instances can be replicated", tierName(in.GetTier()))
	}
	a, err := s.instanceLocked(peer)
	if err != nil {
		return nil, err
	}
	if a.I.GetReplication() != nil {
		return nil, apierror.FailedPrecondition("Instance %s already has a replica, or is one: only a single replica is supported", peer)
	}
	if a.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: a replica can be made only of a READY instance", peer, a.I.GetState())
	}
	if a.I.GetTier() != in.GetTier() || a.I.GetProtocol() != in.GetProtocol() {
		return nil, apierror.InvalidArgument("A replica must have its active instance's tier and protocol: %s %s", tierName(a.I.GetTier()), a.I.GetProtocol())
	}
	if a.Multi != nil || len(in.GetFileShares()) == 0 {
		return nil, apierror.InvalidArgument("Instance %s is a multishare instance: replication is not supported for multishare instances", peer)
	}
	if a.I.GetFileShares()[0].GetCapacityGb() != in.GetFileShares()[0].GetCapacityGb() || a.I.GetFileShares()[0].GetName() != in.GetFileShares()[0].GetName() {
		return nil, apierror.InvalidArgument("A replica's file share must have its active instance's name and capacity: %s, %d GiB",
			a.I.GetFileShares()[0].GetName(), a.I.GetFileShares()[0].GetCapacityGb())
	}
	if projectOf(peer) != projectOf(in.GetName()) {
		return nil, apierror.InvalidArgument("The active instance must be in the replica's project")
	}
	in.Replication = &filestorepb.Replication{Role: filestorepb.Replication_STANDBY,
		Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: peer, State: filestorepb.ReplicaConfig_CREATING}}}
	return a, nil
}

// pair links a new replica to its active instance once it is made.
func pair(active, standby *instRecord, synced time.Time) {
	ts := timestamppb.New(synced)
	active.I.Replication = &filestorepb.Replication{Role: filestorepb.Replication_ACTIVE,
		Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: standby.I.GetName(), State: filestorepb.ReplicaConfig_READY,
			LastActiveSyncTime: ts}}}
	standby.I.Replication = &filestorepb.Replication{Role: filestorepb.Replication_STANDBY,
		Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: active.I.GetName(), State: filestorepb.ReplicaConfig_READY,
			LastActiveSyncTime: ts}}}
}

func isStandby(in *filestorepb.Instance) bool {
	return in.GetReplication().GetRole() == filestorepb.Replication_STANDBY
}

func peerOf(in *filestorepb.Instance) string {
	if rs := in.GetReplication().GetReplicas(); len(rs) > 0 {
		return rs[0].GetPeerInstance()
	}
	return ""
}

// replicate copies each active instance's share to its replica.
func (s *Server) replicate(ctx context.Context) {
	if s.opts.Backend == nil {
		return
	}
	type job struct{ a, b *instRecord }
	s.mu.Lock()
	var jobs []job
	for name, r := range s.instances {
		if r.I.GetReplication().GetRole() != filestorepb.Replication_ACTIVE || r.I.GetState() != filestorepb.Instance_READY || s.busy[name] != "" {
			continue
		}
		b, ok := s.instances[peerOf(r.I)]
		if !ok || b.I.GetState() != filestorepb.Instance_READY || s.busy[b.I.GetName()] != "" {
			continue
		}
		jobs = append(jobs, job{r, b})
	}
	s.mu.Unlock()
	for _, j := range jobs {
		s.syncPair(ctx, j.a, j.b)
	}
}

// syncPair copies a's share to b's and records when.
func (s *Server) syncPair(ctx context.Context, a, b *instRecord) {
	s.mu.Lock()
	an, bn := a.I.GetName(), b.I.GetName()
	if s.busy[an] != "" || s.busy[bn] != "" {
		s.mu.Unlock()
		return
	}
	s.busy[an], s.busy[bn] = "replication", "replication"
	from, to, share := a.Key, b.Key, a.I.GetFileShares()[0].GetName()
	s.mu.Unlock()
	at := s.now()
	err := s.opts.Backend.Replicate(ctx, from, to, share)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, an)
	delete(s.busy, bn)
	state := filestorepb.ReplicaConfig_READY
	var reasons []filestorepb.ReplicaConfig_StateReason
	if err != nil {
		state, reasons = filestorepb.ReplicaConfig_FAILED, []filestorepb.ReplicaConfig_StateReason{filestorepb.ReplicaConfig_PEER_INSTANCE_UNREACHABLE}
	}
	for _, r := range []*instRecord{a, b} {
		rs := r.I.GetReplication().GetReplicas()
		if len(rs) == 0 {
			continue
		}
		rs[0].State, rs[0].StateReasons = state, reasons
		if err == nil {
			rs[0].LastActiveSyncTime = timestamppb.New(at)
		}
	}
	_ = s.saveLocked()
}

// PromoteReplica promotes a standby instance: called on it, or on its
// active instance with peer_instance naming it.
func (s *Server) PromoteReplica(_ context.Context, req *filestorepb.PromoteReplicaRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if r.I.GetReplication() == nil {
		return nil, apierror.FailedPrecondition("Instance %s is not replicated: it has no replica to promote", r.I.GetName())
	}
	standby := r
	if !isStandby(r.I) {
		if req.GetPeerInstance() == "" {
			return nil, apierror.InvalidArgument("peer_instance is required when PromoteReplica is called on an active instance")
		}
		if req.GetPeerInstance() != peerOf(r.I) {
			return nil, apierror.InvalidArgument("peer_instance %q is not the replica of %s", req.GetPeerInstance(), r.I.GetName())
		}
		if standby, err = s.instanceLocked(req.GetPeerInstance()); err != nil {
			return nil, err
		}
	} else if req.GetPeerInstance() != "" && req.GetPeerInstance() != r.I.GetName() {
		return nil, apierror.InvalidArgument("peer_instance %q is not the standby instance %s", req.GetPeerInstance(), r.I.GetName())
	}
	active := s.instances[peerOf(standby.I)]
	if err := s.busyLocked(standby.I.GetName()); err != nil {
		return nil, err
	}
	if standby.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: a replica can be promoted only when READY", standby.I.GetName(), standby.I.GetState())
	}
	standby.I.State = filestorepb.Instance_PROMOTING
	_ = s.saveLocked()
	next := proto.Clone(standby.I).(*filestorepb.Instance)
	next.Replication = nil
	key, ip, spec := standby.Key, standby.IP, standby.specFor(next)
	b := s.opts.Backend
	return s.startOpLocked(locationOf(standby.I.GetName()), standby.I.GetName(), "promote", func(ctx context.Context) error {
		_, err := s.ensure(ctx, b, standby, key, ip, spec)
		return err
	}, func(err error) proto.Message {
		standby.I.State = filestorepb.Instance_READY
		if err != nil {
			return nil
		}
		standby.I.Replication = nil
		if active != nil && s.instances[active.I.GetName()] == active {
			active.I.Replication = nil
		}
		return cloneInst(standby.I)
	}), nil
}
