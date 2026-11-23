package filestore

import (
	"context"
	"strings"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// CreateSnapshot copies an instance's share as a snapshot: ZONAL,
// REGIONAL, ENTERPRISE and HIGH_SCALE_SSD instances, 240 at a time.
func (s *Server) CreateSnapshot(_ context.Context, req *filestorepb.CreateSnapshotRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	if !idRE.MatchString(req.GetSnapshotId()) {
		return nil, apierror.InvalidArgument("snapshot_id %q is invalid: it must be 1 to 63 characters, start with a lowercase letter, contain only lowercase letters, digits and dashes, and end with a letter or digit", req.GetSnapshotId())
	}
	sn := proto.Clone(req.GetSnapshot()).(*filestorepb.Snapshot)
	if sn == nil {
		sn = &filestorepb.Snapshot{}
	}
	if err := checkLabels(sn.GetLabels()); err != nil {
		return nil, err
	}
	if err := s.checkTags(req.GetParent()+"/snapshots/"+req.GetSnapshotId(), sn.GetTags()); err != nil {
		return nil, err
	}
	tags := sn.GetTags()
	sn.Tags = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetParent())
	if err != nil {
		return nil, err
	}
	if tiers[r.I.GetTier()].basic {
		return nil, apierror.InvalidArgument("Snapshots are not supported for tier %s: only zonal, regional and enterprise instances support snapshots", tierName(r.I.GetTier()))
	}
	name := req.GetParent() + "/snapshots/" + req.GetSnapshotId()
	if _, ok := s.snapshots[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	n := 0
	for k := range s.snapshots {
		if strings.HasPrefix(k, req.GetParent()+"/snapshots/") {
			n++
		}
	}
	if n >= MaxSnapshots {
		return nil, apierror.FailedPrecondition("Instance %s has %d snapshots: you can have up to %d snapshots per instance at a time", req.GetParent(), n, MaxSnapshots)
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: a snapshot can be taken only when it is READY", r.I.GetName(), r.I.GetState())
	}
	now := s.now()
	sn.Name, sn.State, sn.CreateTime, sn.FilesystemUsedBytes = name, filestorepb.Snapshot_CREATING, timestamppb.New(now), 0
	rec := &snapRecord{S: sn, Key: keyOf("snap-", name, now)}
	s.snapshots[name] = rec
	_ = s.saveLocked()
	key, share, snapKey := r.Key, r.I.GetFileShares()[0].GetName(), rec.Key
	b := s.opts.Backend
	var used int64
	return s.startOpLocked(locationOf(name), name, "create", func(ctx context.Context) error {
		if err := b.Snapshot(ctx, key, share, snapKey); err != nil {
			return err
		}
		used, _ = b.Usage(ctx, key, share)
		return nil
	}, func(err error) proto.Message {
		if err != nil {
			delete(s.snapshots, name)
			return nil
		}
		rec.S.State, rec.S.FilesystemUsedBytes = filestorepb.Snapshot_READY, used
		s.bindTags(name, tags)
		return proto.Clone(rec.S)
	}), nil
}

func (s *Server) snapshotLocked(name string) (*snapRecord, error) {
	if !snapshotRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/instances/{instance}/snapshots/{snapshot}", name)
	}
	r, ok := s.snapshots[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return r, nil
}

// GetSnapshot answers a snapshot.
func (s *Server) GetSnapshot(_ context.Context, req *filestorepb.GetSnapshotRequest) (*filestorepb.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.snapshotLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(r.S).(*filestorepb.Snapshot), nil
}

// ListSnapshots lists an instance's snapshots.
func (s *Server) ListSnapshots(_ context.Context, req *filestorepb.ListSnapshotsRequest) (*filestorepb.ListSnapshotsResponse, error) {
	s.mu.Lock()
	if _, err := s.instanceLocked(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var items []*filestorepb.Snapshot
	for _, name := range sortedKeys(s.snapshots) {
		if strings.HasPrefix(name, req.GetParent()+"/snapshots/") {
			items = append(items, proto.Clone(s.snapshots[name].S).(*filestorepb.Snapshot))
		}
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("snapshots:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &filestorepb.ListSnapshotsResponse{Snapshots: page, NextPageToken: next}, nil
}

// UpdateSnapshot changes a snapshot's description and labels.
func (s *Server) UpdateSnapshot(_ context.Context, req *filestorepb.UpdateSnapshotRequest) (*longrunningpb.Operation, error) {
	in := req.GetSnapshot()
	if in == nil {
		return nil, apierror.InvalidArgument("snapshot is required")
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.snapshotLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(r.S).(*filestorepb.Snapshot)
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		default:
			return nil, apierror.InvalidArgument("Field %q is not an updatable field of a snapshot", p)
		}
	}
	return s.startOpLocked(locationOf(r.S.GetName()), r.S.GetName(), "update", func(context.Context) error { return nil },
		func(error) proto.Message {
			r.S = next
			return proto.Clone(r.S)
		}), nil
}

// DeleteSnapshot deletes a snapshot's copy.
func (s *Server) DeleteSnapshot(_ context.Context, req *filestorepb.DeleteSnapshotRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.snapshotLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.busyLocked(r.S.GetName()); err != nil {
		return nil, err
	}
	inst := s.instances[locationOf(r.S.GetName())+"/instances/"+strings.Split(r.S.GetName(), "/")[5]]
	if inst == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", r.S.GetName())
	}
	r.S.State = filestorepb.Snapshot_DELETING
	key, share, snapKey := inst.Key, inst.I.GetFileShares()[0].GetName(), r.Key
	b := s.opts.Backend
	return s.startOpLocked(locationOf(r.S.GetName()), r.S.GetName(), "delete", func(ctx context.Context) error {
		return b.DeleteSnapshot(ctx, key, share, snapKey)
	}, func(err error) proto.Message {
		if err != nil {
			r.S.State = filestorepb.Snapshot_READY
			return nil
		}
		delete(s.snapshots, r.S.GetName())
		s.unbindTags(r.S.GetName())
		return nil
	}), nil
}
