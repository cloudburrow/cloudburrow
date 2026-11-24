package filestore

import (
	"context"
	"strings"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// CreateBackup copies an instance's share out as a regional backup, kept
// after the instance is deleted.
func (s *Server) CreateBackup(ctx context.Context, req *filestorepb.CreateBackupRequest) (*longrunningpb.Operation, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if !isRegion(m[2]) {
		return nil, apierror.InvalidArgument("Backups are regional resources: the location must be a region, not %q", m[2])
	}
	if !idRE.MatchString(req.GetBackupId()) {
		return nil, apierror.InvalidArgument("backup_id %q is invalid: it must be 1 to 63 characters, start with a lowercase letter, contain only lowercase letters, digits and dashes, and end with a letter or digit", req.GetBackupId())
	}
	bk := proto.Clone(req.GetBackup()).(*filestorepb.Backup)
	if bk == nil {
		return nil, apierror.InvalidArgument("backup is required")
	}
	if err := checkLabels(bk.GetLabels()); err != nil {
		return nil, err
	}
	if err := s.checkTags(req.GetParent()+"/backups/"+req.GetBackupId(), bk.GetTags()); err != nil {
		return nil, err
	}
	tags := bk.GetTags()
	bk.Tags = nil
	if k := bk.GetKmsKey(); k != "" {
		if err := s.checkKey(ctx, "kms_key", k, m[1], m[2]); err != nil {
			return nil, err
		}
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bk.GetSourceInstance() == "" || bk.GetSourceFileShare() == "" {
		return nil, apierror.InvalidArgument("backup.source_instance and backup.source_file_share are required")
	}
	r, err := s.instanceLocked(bk.GetSourceInstance())
	if err != nil {
		return nil, err
	}
	if strings.Split(bk.GetSourceInstance(), "/")[1] != m[1] {
		return nil, apierror.InvalidArgument("The source instance must be in the backup's project")
	}
	if bk.GetKmsKey() != "" && tiers[r.I.GetTier()].basic {
		return nil, apierror.InvalidArgument("Customer-managed encryption keys are not supported for backups of tier %s instances", tierName(r.I.GetTier()))
	}
	fs := r.I.GetFileShares()[0]
	if fs.GetName() != bk.GetSourceFileShare() {
		return nil, apierror.InvalidArgument("source_file_share %q is not a file share of %s; its share is %q", bk.GetSourceFileShare(), r.I.GetName(), fs.GetName())
	}
	name := req.GetParent() + "/backups/" + req.GetBackupId()
	if _, ok := s.backups[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: a backup can be made only when it is READY", r.I.GetName(), r.I.GetState())
	}
	now := s.now()
	bk.Name, bk.State, bk.CreateTime = name, filestorepb.Backup_CREATING, timestamppb.New(now)
	bk.CapacityGb, bk.SourceInstanceTier, bk.FileSystemProtocol = fs.GetCapacityGb(), r.I.GetTier(), r.I.GetProtocol()
	bk.StorageBytes, bk.DownloadBytes = 0, 0
	bk.SatisfiesPzs, bk.SatisfiesPzi = wrapperspb.Bool(false), false
	rec := &backupRecord{B: bk, Key: keyOf("backup-", name, now)}
	s.backups[name] = rec
	_ = s.saveLocked()
	key, share, backupKey := r.Key, fs.GetName(), rec.Key
	b := s.opts.Backend
	var size int64
	// The instance is busy while its files are read, as on Google, where
	// the instance shows the backup's operation.
	s.busy[r.I.GetName()] = "backup " + name
	instName := r.I.GetName()
	kmsKey := bk.GetKmsKey()
	var sealed *cmek.Sealed
	return s.startOpLocked(req.GetParent(), name, "create", func(ctx context.Context) error {
		var dek []byte
		if kmsKey != "" {
			var err error
			if dek, sealed, err = s.sealKey(ctx, m[1], kmsKey, name); err != nil {
				return err
			}
		}
		n, err := b.Backup(ctx, key, share, backupKey, dek)
		size = n
		return err
	}, func(err error) proto.Message {
		delete(s.busy, instName)
		if err != nil {
			delete(s.backups, name)
			return nil
		}
		rec.Sealed = sealed
		s.bindTags(name, tags)
		rec.B.State, rec.B.StorageBytes, rec.B.DownloadBytes = filestorepb.Backup_READY, size, size
		return proto.Clone(rec.B)
	}), nil
}

func (s *Server) backupLocked(name string) (*backupRecord, error) {
	if !backupRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/backups/{backup}", name)
	}
	r, ok := s.backups[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return r, nil
}

// GetBackup answers a backup.
func (s *Server) GetBackup(_ context.Context, req *filestorepb.GetBackupRequest) (*filestorepb.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.backupLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(r.B).(*filestorepb.Backup), nil
}

// ListBackups lists a region's backups, or every region's with "-".
func (s *Server) ListBackups(_ context.Context, req *filestorepb.ListBackupsRequest) (*filestorepb.ListBackupsResponse, error) {
	m, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if m[2] != "-" && !isRegion(m[2]) {
		return nil, apierror.InvalidArgument("Backups are regional resources: the location must be a region or -, not %q", m[2])
	}
	s.mu.Lock()
	var items []*filestorepb.Backup
	for _, name := range sortedKeys(s.backups) {
		if (m[2] == "-" && strings.HasPrefix(name, "projects/"+m[1]+"/locations/")) || strings.HasPrefix(name, req.GetParent()+"/backups/") {
			items = append(items, proto.Clone(s.backups[name].B).(*filestorepb.Backup))
		}
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("backups:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &filestorepb.ListBackupsResponse{Backups: page, NextPageToken: next}, nil
}

// UpdateBackup changes a backup's description and labels.
func (s *Server) UpdateBackup(_ context.Context, req *filestorepb.UpdateBackupRequest) (*longrunningpb.Operation, error) {
	in := req.GetBackup()
	if in == nil {
		return nil, apierror.InvalidArgument("backup is required")
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.backupLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(r.B).(*filestorepb.Backup)
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
			return nil, apierror.InvalidArgument("Field %q is not an updatable field of a backup", p)
		}
	}
	return s.startOpLocked(locationOf(r.B.GetName()), r.B.GetName(), "update", func(context.Context) error { return nil },
		func(error) proto.Message {
			r.B = next
			return proto.Clone(r.B)
		}), nil
}

// DeleteBackup deletes a backup's data.
func (s *Server) DeleteBackup(_ context.Context, req *filestorepb.DeleteBackupRequest) (*longrunningpb.Operation, error) {
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.backupLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.busyLocked(r.B.GetName()); err != nil {
		return nil, err
	}
	for _, in := range s.instances {
		if in.I.GetState() == filestorepb.Instance_CREATING || in.I.GetState() == filestorepb.Instance_RESTORING {
			if s.busy[in.I.GetName()] != "" && in.I.GetFileShares()[0].GetSourceBackup() == r.B.GetName() {
				return nil, apierror.FailedPrecondition("Backup %s is being restored to %s", r.B.GetName(), in.I.GetName())
			}
		}
	}
	r.B.State = filestorepb.Backup_DELETING
	backupKey := r.Key
	b := s.opts.Backend
	return s.startOpLocked(locationOf(r.B.GetName()), r.B.GetName(), "delete", func(ctx context.Context) error {
		return b.DeleteBackup(ctx, backupKey)
	}, func(err error) proto.Message {
		if err != nil {
			r.B.State = filestorepb.Backup_READY
			return nil
		}
		delete(s.backups, r.B.GetName())
		s.unbindTags(r.B.GetName())
		return nil
	}), nil
}

// Backups are the backups, for the console.
func (s *Server) Backups() []*filestorepb.Backup {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*filestorepb.Backup
	for _, name := range sortedKeys(s.backups) {
		out = append(out, proto.Clone(s.backups[name].B).(*filestorepb.Backup))
	}
	return out
}

// Snapshots are an instance's snapshots, for the console.
func (s *Server) Snapshots(instance string) []*filestorepb.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*filestorepb.Snapshot
	for _, name := range sortedKeys(s.snapshots) {
		if strings.HasPrefix(name, instance+"/snapshots/") {
			out = append(out, proto.Clone(s.snapshots[name].S).(*filestorepb.Snapshot))
		}
	}
	return out
}
