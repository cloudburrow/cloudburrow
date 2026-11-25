package filestore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

func (s *Server) noBackend() error {
	if s.opts.Backend == nil {
		return apierror.FailedPrecondition("Filestore instances need the NFS server backend, which runs in Docker beside the instance's cluster; start CloudBurrow with --services filestore")
	}
	return nil
}

// parent checks a projects/P/locations/L parent.
func checkParent(parent string) ([]string, error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
	}
	return m, nil
}

// CreateInstance creates an instance and starts its NFS server.
func (s *Server) CreateInstance(ctx context.Context, req *filestorepb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	return s.createInstance(ctx, req, nil, nil)
}

// createInstance creates an instance of one file share, or a multishare
// instance (multi, from v1beta1), its operation's answer made by answer
// (the v1 instance when nil).
func (s *Server) createInstance(ctx context.Context, req *filestorepb.CreateInstanceRequest, multi *multiState, answer func(*filestorepb.Instance) proto.Message) (*longrunningpb.Operation, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	loc := m[2]
	if !validLocation(loc) {
		return nil, apierror.InvalidArgument("Location %q is not a valid Filestore location", loc)
	}
	if !idRE.MatchString(req.GetInstanceId()) {
		return nil, apierror.InvalidArgument("instance_id %q is invalid: it must be 1 to 63 characters, start with a lowercase letter, contain only lowercase letters, digits and dashes, and end with a letter or digit", req.GetInstanceId())
	}
	in := proto.Clone(req.GetInstance()).(*filestorepb.Instance)
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	name := req.GetParent() + "/instances/" + req.GetInstanceId()
	info, ok := tiers[in.GetTier()]
	if !ok {
		return nil, apierror.InvalidArgument("instance.tier is required: one of BASIC_HDD, BASIC_SSD, ZONAL, REGIONAL, ENTERPRISE")
	}
	if info.regional && !isRegion(loc) {
		return nil, apierror.InvalidArgument("Tier %s instances are regional: the location must be a region, not %q", tierName(in.GetTier()), loc)
	}
	if !info.regional && in.GetTier() != filestorepb.Instance_ZONAL && !isZone(loc) {
		return nil, apierror.InvalidArgument("Tier %s instances are zonal: the location must be a zone, not %q", tierName(in.GetTier()), loc)
	}
	if in.GetTier() == filestorepb.Instance_ZONAL && !isZone(loc) {
		return nil, apierror.InvalidArgument("Tier ZONAL instances are zonal: the location must be a zone, not %q", loc)
	}
	switch in.GetProtocol() {
	case filestorepb.Instance_FILE_PROTOCOL_UNSPECIFIED:
		in.Protocol = filestorepb.Instance_NFS_V3
	case filestorepb.Instance_NFS_V3:
	case filestorepb.Instance_NFS_V4_1:
		if info.basic {
			return nil, apierror.InvalidArgument("Protocol NFS_V4_1 is not supported for tier %s: basic tier instances support NFSv3 only", tierName(in.GetTier()))
		}
	default:
		return nil, apierror.InvalidArgument("Protocol %s is not supported", in.GetProtocol())
	}
	fs := &filestorepb.FileShareConfig{}
	if multi != nil {
		if err := multi.checkNew(in); err != nil {
			return nil, err
		}
	} else {
		if len(in.GetFileShares()) != 1 {
			return nil, apierror.InvalidArgument("file_shares must have exactly one entry: for this version, only a single file share is supported")
		}
		fs = in.FileShares[0]
		if err := checkShareName(in.GetTier(), fs.GetName()); err != nil {
			return nil, err
		}
		if err := checkCapacity(in.GetTier(), fs.GetCapacityGb()); err != nil {
			return nil, err
		}
		if err := checkExports(fs.GetNfsExportOptions()); err != nil {
			return nil, err
		}
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if err := s.checkTags(name, in.GetTags()); err != nil {
		return nil, err
	}
	tags := in.GetTags()
	in.Tags = nil
	if k := in.GetKmsKeyName(); k != "" {
		if info.basic {
			return nil, apierror.InvalidArgument("Customer-managed encryption keys are not supported for tier %s: only zonal, regional and enterprise instances support them", tierName(in.GetTier()))
		}
		if err := s.checkKey(ctx, "kms_key_name", k, m[1], loc); err != nil {
			return nil, err
		}
	}
	if in.GetPerformanceConfig() != nil && in.GetTier() != filestorepb.Instance_ZONAL && in.GetTier() != filestorepb.Instance_REGIONAL {
		return nil, apierror.InvalidArgument("performance_config is supported only by the ZONAL and REGIONAL tiers")
	}
	if !in.GetDeletionProtectionEnabled() && in.GetDeletionProtectionReason() != "" {
		return nil, apierror.InvalidArgument("deletion_protection_reason can be set only with deletion_protection_enabled")
	}
	if err := s.checkNetwork(in, m[1], name); err != nil {
		return nil, err
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	in.Name = name
	active, err := s.checkReplicaLocked(in)
	if err != nil {
		return nil, err
	}
	var source *backupRecord
	if src := fs.GetSourceBackup(); src != "" {
		b, err := s.restorableLocked(src, in.GetTier(), in.GetProtocol(), fs.GetCapacityGb(), true)
		if err != nil {
			return nil, err
		}
		source = b
	}
	bdrSrc, bdrName, err := s.bdrSourceLocked(in)
	if err != nil {
		return nil, err
	}
	now := s.now()
	in.Name = name
	in.State = filestorepb.Instance_CREATING
	in.StatusMessage = ""
	in.CreateTime = timestamppb.New(now)
	in.SatisfiesPzs = wrapperspb.Bool(false)
	in.SatisfiesPzi = false
	in.SuspensionReasons = nil
	in.CustomPerformanceSupported = in.GetTier() == filestorepb.Instance_ZONAL || in.GetTier() == filestorepb.Instance_REGIONAL
	rec := &instRecord{I: in, Key: keyOf("fs-", name, now), Multi: multi}
	rec.setLimits(in)
	if err := s.placeLocked(in, m[1], rec); err != nil {
		return nil, err
	}
	s.instances[name] = rec
	if err := s.saveLocked(); err != nil {
		delete(s.instances, name)
		return nil, err
	}
	key, spec, share := rec.Key, rec.spec(), fs.GetName()
	var backupKey, backupName string
	var sealed *cmek.Sealed
	if source != nil {
		backupKey, backupName, sealed = source.Key, source.B.GetName(), source.Sealed
	}
	bdrDone := func(error) {}
	if bdrSrc != nil {
		backupKey = bdrSrc.Key
		job := s.bdrAPI.RestoreStarted("RESTORE", bdrName, m[1], name, "")
		api := s.bdrAPI
		bdrDone = func(err error) { api.RestoreFinished(job, err) }
	}
	var activeKey string
	if active != nil {
		activeKey = active.Key
		s.busy[active.I.GetName()] = "replica " + name
	}
	var synced time.Time
	b := s.opts.Backend
	return s.startOpLocked(req.GetParent(), name, "create", func(ctx context.Context) error {
		ip, err := s.ensure(ctx, b, rec, key, "", spec)
		if err != nil {
			var ae *apierror.Error
			if errors.As(err, &ae) {
				return err
			}
			return apierror.Internal(err, "The NFS server did not start: %v", err)
		}
		s.mu.Lock()
		rec.IP = ip
		s.mu.Unlock()
		if backupKey != "" {
			var dek []byte
			if backupName != "" {
				var err error
				if dek, err = s.openKey(ctx, projectOf(backupName), backupName, sealed); err != nil {
					return err
				}
			}
			if err := b.Restore(ctx, key, share, backupKey, dek); err != nil {
				return apierror.Internal(err, "Restoring the backup failed: %v", err)
			}
		}
		if activeKey != "" {
			synced = s.now()
			if err := b.Replicate(ctx, activeKey, key, share); err != nil {
				return apierror.Internal(err, "Copying the active instance's data failed: %v", err)
			}
		}
		return nil
	}, func(err error) proto.Message {
		bdrDone(err)
		if active != nil {
			delete(s.busy, active.I.GetName())
		}
		if err != nil {
			rec.I.State, rec.I.StatusMessage = filestorepb.Instance_ERROR, apierror.From(err).Message
			return nil
		}
		rec.setIP()
		rec.I.State = filestorepb.Instance_READY
		if active != nil && s.instances[active.I.GetName()] == active {
			pair(active, rec, synced)
		}
		s.bindTags(name, tags)
		if answer != nil {
			return answer(cloneInst(rec.I))
		}
		return cloneInst(rec.I)
	}), nil
}

func (s *Server) instanceLocked(name string) (*instRecord, error) {
	if !instanceRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/instances/{instance}", name)
	}
	r, ok := s.instances[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return r, nil
}

// GetInstance answers an instance.
func (s *Server) GetInstance(_ context.Context, req *filestorepb.GetInstanceRequest) (*filestorepb.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return cloneInst(r.I), nil
}

// ListInstances lists a location's instances, or every location's with
// "-".
func (s *Server) ListInstances(_ context.Context, req *filestorepb.ListInstancesRequest) (*filestorepb.ListInstancesResponse, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if m[2] != "-" && !validLocation(m[2]) {
		return nil, apierror.InvalidArgument("Location %q is not a valid Filestore location", m[2])
	}
	s.mu.Lock()
	var items []*filestorepb.Instance
	for _, name := range sortedKeys(s.instances) {
		if (m[2] == "-" && strings.HasPrefix(name, "projects/"+m[1]+"/locations/")) || strings.HasPrefix(name, req.GetParent()+"/instances/") {
			items = append(items, cloneInst(s.instances[name].I))
		}
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("instances:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &filestorepb.ListInstancesResponse{Instances: page, NextPageToken: next}, nil
}

// UpdateInstance changes the fields the mask names: description, labels,
// file_shares (capacity and export options), deletion protection and
// performance_config.
func (s *Server) UpdateInstance(_ context.Context, req *filestorepb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: it can be updated only when READY", r.I.GetName(), r.I.GetState())
	}
	if in.GetEtag() != "" && in.GetEtag() != etagOf(r.I) {
		return nil, apierror.Aborted("The etag %q does not match the instance's current etag", in.GetEtag())
	}
	next := proto.Clone(r.I).(*filestorepb.Instance)
	reconfigure := false
	for _, p := range paths {
		switch p {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "file_shares", "fileShares":
			if r.I.GetReplication() != nil {
				return nil, apierror.FailedPrecondition("Instance %s is replicated: its file share cannot be changed while it has a replica or is one", r.I.GetName())
			}
			if r.Multi != nil {
				return nil, apierror.InvalidArgument("Instance %s is a multishare instance: its shares are changed through its shares, not file_shares", r.I.GetName())
			}
			if len(in.GetFileShares()) != 1 {
				return nil, apierror.InvalidArgument("file_shares must have exactly one entry")
			}
			fs, cur := proto.Clone(in.FileShares[0]).(*filestorepb.FileShareConfig), r.I.FileShares[0]
			if fs.GetName() != cur.GetName() {
				return nil, apierror.InvalidArgument("The file share's name cannot be changed: it is %q", cur.GetName())
			}
			if fs.GetSourceBackup() != "" && fs.GetSourceBackup() != cur.GetSourceBackup() {
				return nil, apierror.InvalidArgument("source_backup cannot be updated: restore a backup with RestoreInstance")
			}
			if err := s.checkResize(r, cur.GetCapacityGb(), fs.GetCapacityGb()); err != nil {
				return nil, err
			}
			if err := checkExports(fs.GetNfsExportOptions()); err != nil {
				return nil, err
			}
			if !proto.Equal(&filestorepb.FileShareConfig{NfsExportOptions: fs.GetNfsExportOptions()}, &filestorepb.FileShareConfig{NfsExportOptions: cur.GetNfsExportOptions()}) {
				reconfigure = true
			}
			fs.Source = cur.Source
			next.FileShares = []*filestorepb.FileShareConfig{fs}
		case "deletion_protection_enabled", "deletionProtectionEnabled":
			next.DeletionProtectionEnabled = in.GetDeletionProtectionEnabled()
			if !next.DeletionProtectionEnabled {
				next.DeletionProtectionReason = ""
			}
		case "deletion_protection_reason", "deletionProtectionReason":
			next.DeletionProtectionReason = in.GetDeletionProtectionReason()
		case "performance_config", "performanceConfig":
			if !r.I.GetCustomPerformanceSupported() {
				return nil, apierror.InvalidArgument("performance_config is supported only by the ZONAL and REGIONAL tiers")
			}
			next.PerformanceConfig = in.GetPerformanceConfig()
		case "tier", "protocol", "networks", "kms_key_name", "name", "create_time", "state":
			return nil, apierror.InvalidArgument("Field %q cannot be updated", p)
		default:
			return nil, apierror.InvalidArgument("Field %q is not an updatable field of an instance", p)
		}
	}
	if !next.GetDeletionProtectionEnabled() && next.GetDeletionProtectionReason() != "" {
		return nil, apierror.InvalidArgument("deletion_protection_reason can be set only with deletion_protection_enabled")
	}
	r.setLimits(next)
	if !reflect.DeepEqual(r.specFor(next), r.spec()) {
		reconfigure = true
	}
	key, ip, spec := r.Key, r.IP, r.specFor(next)
	b := s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "update", func(ctx context.Context) error {
		if !reconfigure {
			return nil
		}
		_, err := s.ensure(ctx, b, r, key, ip, spec)
		return err
	}, func(err error) proto.Message {
		if err != nil {
			return nil
		}
		next.State = r.I.GetState()
		r.I = next
		return cloneInst(r.I)
	}), nil
}

// checkResize checks a capacity change: basic tiers grow only, the others
// stay in their range, and none shrinks below the bytes the share holds.
func (s *Server) checkResize(r *instRecord, from, to int64) error {
	if from == to {
		return nil
	}
	t := r.I.GetTier()
	if err := checkCapacity(t, to); err != nil {
		return err
	}
	if tiers[t].basic && to < from {
		return apierror.InvalidArgument("Basic tier instances can only be scaled up: capacity_gb %d is less than the current %d", to, from)
	}
	if bandOf(t, to) != bandOf(t, from) {
		return apierror.InvalidArgument("capacity_gb %d is outside the instance's capacity range: an instance stays in the range it was created in; create a new instance for greater or lower capacity", to)
	}
	return nil
}

func locationOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

// DeleteInstance deletes an instance, its NFS server and its data; with
// snapshots it needs force, which deletes them too.
func (s *Server) DeleteInstance(_ context.Context, req *filestorepb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetDeletionProtectionEnabled() {
		return nil, apierror.FailedPrecondition("Instance %s has deletion protection enabled; disable it before deleting the instance", r.I.GetName())
	}
	var snaps []string
	for name := range s.snapshots {
		if strings.HasPrefix(name, r.I.GetName()+"/snapshots/") {
			snaps = append(snaps, name)
		}
	}
	var peer *instRecord
	if r.I.GetReplication() != nil {
		if !isStandby(r.I) {
			return nil, apierror.FailedPrecondition("Instance %s has a replica, %s: delete the replica, or promote it, first", r.I.GetName(), peerOf(r.I))
		}
		peer = s.instances[peerOf(r.I)]
	}
	if len(snaps) > 0 && !req.GetForce() {
		return nil, apierror.FailedPrecondition("Instance %s has %d snapshot(s): delete them first, or delete with force to delete them too", r.I.GetName(), len(snaps))
	}
	r.I.State = filestorepb.Instance_DELETING
	_ = s.saveLocked()
	key := r.Key
	b := s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "delete", func(ctx context.Context) error {
		return b.Remove(ctx, key)
	}, func(err error) proto.Message {
		if err != nil {
			r.I.State, r.I.StatusMessage = filestorepb.Instance_ERROR, "Deleting the NFS server failed: "+err.Error()
			return nil
		}
		for _, sn := range snaps {
			delete(s.snapshots, sn)
			s.unbindTags(sn)
		}
		delete(s.instances, r.I.GetName())
		if peer != nil && s.instances[peer.I.GetName()] == peer {
			peer.I.Replication = nil
		}
		s.unbindTags(r.I.GetName())
		return nil
	}), nil
}

// restorableLocked checks backup name can be restored into an instance of
// tier, protocol and capacity, new or existing.
func (s *Server) restorableLocked(name string, tier filestorepb.Instance_Tier, protocol filestorepb.Instance_FileProtocol, capacity int64, newInstance bool) (*backupRecord, error) {
	if !backupRE.MatchString(name) {
		return nil, apierror.InvalidArgument("source_backup %q must be projects/{project}/locations/{location}/backups/{backup}", name)
	}
	b, ok := s.backups[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	if b.B.GetState() != filestorepb.Backup_READY {
		return nil, apierror.FailedPrecondition("Backup %s is %s: only a READY backup can be restored", name, b.B.GetState())
	}
	srcBasic := tiers[b.B.GetSourceInstanceTier()].basic
	if srcBasic != tiers[tier].basic {
		if srcBasic {
			return nil, apierror.InvalidArgument("A backup of a basic tier instance can be restored only to a BASIC_HDD or BASIC_SSD instance")
		}
		return nil, apierror.InvalidArgument("A backup of a %s instance cannot be restored to a basic tier instance", tierName(b.B.GetSourceInstanceTier()))
	}
	if !srcBasic && !newInstance {
		return nil, apierror.InvalidArgument("A backup of a %s instance can be restored only to a new instance", tierName(b.B.GetSourceInstanceTier()))
	}
	if p := b.B.GetFileSystemProtocol(); p != filestorepb.Instance_FILE_PROTOCOL_UNSPECIFIED && p != protocol {
		return nil, apierror.InvalidArgument("The instance must use the same protocol as the backup's source instance, %s", p)
	}
	if capacity < b.B.GetCapacityGb() {
		return nil, apierror.InvalidArgument("The file share's capacity, %d GiB, must be equal to or exceed the backup's source capacity, %d GiB", capacity, b.B.GetCapacityGb())
	}
	return b, nil
}

// RestoreInstance replaces a basic tier instance's file share's files with
// a backup's.
func (s *Server) RestoreInstance(_ context.Context, req *filestorepb.RestoreInstanceRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetSourceBackup() == "" {
		return nil, apierror.InvalidArgument("source_backup is required")
	}
	if err := multiRefused(r); err != nil {
		return nil, err
	}
	fs := r.I.GetFileShares()[0]
	if req.GetFileShare() != fs.GetName() {
		return nil, apierror.InvalidArgument("file_share %q is not a file share of %s; its share is %q", req.GetFileShare(), r.I.GetName(), fs.GetName())
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: it can be restored only when READY", r.I.GetName(), r.I.GetState())
	}
	b, err := s.restorableLocked(req.GetSourceBackup(), r.I.GetTier(), r.I.GetProtocol(), fs.GetCapacityGb(), false)
	if err != nil {
		return nil, err
	}
	r.I.State = filestorepb.Instance_RESTORING
	_ = s.saveLocked()
	key, share, backupKey, src, sealed := r.Key, fs.GetName(), b.Key, req.GetSourceBackup(), b.Sealed
	be := s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "restore", func(ctx context.Context) error {
		dek, err := s.openKey(ctx, projectOf(src), src, sealed)
		if err != nil {
			return err
		}
		return be.Restore(ctx, key, share, backupKey, dek)
	}, func(err error) proto.Message {
		r.I.State = filestorepb.Instance_READY
		if err != nil {
			r.I.StatusMessage = "Restoring the backup failed: " + err.Error()
			return nil
		}
		r.I.StatusMessage = ""
		r.I.FileShares[0].Source = &filestorepb.FileShareConfig_SourceBackup{SourceBackup: src}
		return cloneInst(r.I)
	}), nil
}

// RevertInstance replaces the instance's files with a snapshot's and
// deletes the snapshots taken after it.
func (s *Server) RevertInstance(_ context.Context, req *filestorepb.RevertInstanceRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if tiers[r.I.GetTier()].basic {
		return nil, apierror.InvalidArgument("Revert is not supported for tier %s: only zonal, regional and enterprise instances can be reverted", tierName(r.I.GetTier()))
	}
	if req.GetTargetSnapshotId() == "" {
		return nil, apierror.InvalidArgument("target_snapshot_id is required")
	}
	snapName := r.I.GetName() + "/snapshots/" + req.GetTargetSnapshotId()
	target, ok := s.snapshots[snapName]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", snapName)
	}
	if target.S.GetState() != filestorepb.Snapshot_READY {
		return nil, apierror.FailedPrecondition("Snapshot %s is %s: only a READY snapshot can be reverted to", snapName, target.S.GetState())
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: it can be reverted only when READY", r.I.GetName(), r.I.GetState())
	}
	var newer []*snapRecord
	for name, sn := range s.snapshots {
		if strings.HasPrefix(name, r.I.GetName()+"/snapshots/") && sn.S.GetCreateTime().AsTime().After(target.S.GetCreateTime().AsTime()) {
			newer = append(newer, sn)
		}
	}
	r.I.State = filestorepb.Instance_REVERTING
	_ = s.saveLocked()
	key, share, snapKey := r.Key, r.I.GetFileShares()[0].GetName(), target.Key
	be := s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "revert", func(ctx context.Context) error {
		if err := be.Revert(ctx, key, share, snapKey); err != nil {
			return err
		}
		for _, sn := range newer {
			if err := be.DeleteSnapshot(ctx, key, share, sn.Key); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) proto.Message {
		r.I.State = filestorepb.Instance_READY
		if err != nil {
			r.I.StatusMessage = "Reverting failed: " + err.Error()
			return nil
		}
		for _, sn := range newer {
			delete(s.snapshots, sn.S.GetName())
		}
		return cloneInst(r.I)
	}), nil
}

// Usage is the bytes an instance's share holds, for the console.
func (s *Server) Usage(ctx context.Context, name string) (int64, error) {
	s.mu.Lock()
	r, ok := s.instances[name]
	if !ok || s.opts.Backend == nil || r.I.GetState() != filestorepb.Instance_READY {
		s.mu.Unlock()
		return 0, apierror.NotFound("Resource '%s' was not found", name)
	}
	if r.Multi != nil {
		var total int64
		key, shares := r.Key, r.Multi.mounts()
		s.mu.Unlock()
		for _, sh := range shares {
			n, err := s.opts.Backend.Usage(ctx, key, sh)
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	}
	key, share := r.Key, r.I.GetFileShares()[0].GetName()
	s.mu.Unlock()
	return s.opts.Backend.Usage(ctx, key, share)
}
