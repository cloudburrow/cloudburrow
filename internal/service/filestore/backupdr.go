package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Backup and DR (#1524). The Backup and DR Service protects an instance
// through a backup plan association of resource type
// file.googleapis.com/Instance
// (docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/filestore/filestore-instance-backup,
// read 2026-11-24). Its "enhanced backups" are offered for the "Zonal,
// Regional, Enterprise" tiers, not the basic ones
// (docs.cloud.google.com/filestore/docs/backups). A vault's backup is a
// copy of the share's files, as a Filestore backup is, kept under the
// run's ID outside the project's backups, so ListBackups does not show it
// and it outlives the instance.
//
// It is restored by creating an instance whose file share names it in
// sourceBackupdrBackup (gcloud filestore instances create --file-share
// source-backupdr-backup=…): "You can only restore an enhanced backup to a
// new instance", and "The provisioned capacity of the new instance must be
// equal to or exceed the provisioned capacity of the source instance"
// (docs.cloud.google.com/filestore/docs/gcbdr-restore-backups). That field
// is in the REST discovery document (google.golang.org/api/file/v1,
// v0.314.0) and in no published filestorepb (cloud.google.com/go/filestore
// through v1.18.1): it is served over REST through the transcoder's
// overlay, as Cloud Monitoring's uptime fields are (#2146).

const (
	fieldSourceBackupDR = "source_backupdr_backup"
	numSourceBackupDR   = 1000001
)

// filestoreOverlay is google.cloud.filestore.v1 with sourceBackupdrBackup.
var filestoreOverlay = rest.MustOverlay("google.cloud.filestore.v1", func(f *descriptorpb.FileDescriptorProto) {
	for _, m := range f.GetMessageType() {
		if m.GetName() != "FileShareConfig" {
			continue
		}
		for _, fd := range m.GetField() {
			if fd.GetName() == fieldSourceBackupDR {
				return
			}
		}
		m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{Name: proto.String(fieldSourceBackupDR),
			JsonName: proto.String("sourceBackupdrBackup"), Number: proto.Int32(numSourceBackupDR),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()})
	}
})

// SourceBackupDR is a file share's sourceBackupdrBackup, "" when unset.
func SourceBackupDR(fs *filestorepb.FileShareConfig) string {
	if fs == nil {
		return ""
	}
	v, ok := filestoreOverlay.Get(fs, protoreflect.Name(fieldSourceBackupDR))
	if !ok {
		return ""
	}
	return v.String()
}

// SetSourceBackupDR sets a file share's sourceBackupdrBackup.
func SetSourceBackupDR(fs *filestorepb.FileShareConfig, backup string) error {
	v := protoreflect.Value{}
	if backup != "" {
		v = protoreflect.ValueOfString(backup)
	}
	return filestoreOverlay.Set(fs, protoreflect.Name(fieldSourceBackupDR), v)
}

// BackupDR is the Backup and DR Service as Filestore's restores see it.
type BackupDR interface {
	// FilestoreBackup is the run a Backup and DR backup of an instance
	// holds: an error when it is no such backup, is not ACTIVE, or its
	// vault's key cannot be used.
	FilestoreBackup(backup string) (runID string, err error)
	// RestoreStarted and RestoreFinished log a restore job (#1924).
	RestoreStarted(category, source, restoreProject, restoreResource, recoveryPoint string) string
	RestoreFinished(jobID string, err error)
}

// SetBackupDR serves sourceBackupdrBackup through b; without it a
// CreateInstance naming one is FAILED_PRECONDITION.
func (s *Server) SetBackupDR(b BackupDR) {
	s.mu.Lock()
	s.bdrAPI = b
	s.mu.Unlock()
}

// bdrBackup is a vault's backup of an instance, as this server keeps it.
type bdrBackup struct {
	Key, Source, Share string
	CapacityGb         int64
	Tier, Protocol     string
	Time               time.Time
}

// BackupDRInstance is what the Backup and DR Service records of an
// instance.
type BackupDRInstance struct {
	Name, CreateTime, Tier string
}

// BackupDRInstance is instance name, for an association: one of a tier
// Backup and DR protects.
func (s *Server) BackupDRInstance(name string) (BackupDRInstance, error) {
	if !instanceRE.MatchString(name) {
		return BackupDRInstance{}, apierror.InvalidArgument("%q is not a Filestore instance: the format is projects/{project}/locations/{location}/instances/{instance}", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.instances[name]
	if !ok {
		return BackupDRInstance{}, apierror.NotFound("The Filestore instance %s does not exist.", name)
	}
	if err := bdrTier(r.I); err != nil {
		return BackupDRInstance{}, err
	}
	return BackupDRInstance{Name: name, CreateTime: r.I.GetCreateTime().AsTime().UTC().Format(time.RFC3339Nano), Tier: tierName(r.I.GetTier())}, nil
}

func bdrTier(in *filestorepb.Instance) error {
	if tiers[in.GetTier()].basic || in.GetTier() == filestorepb.Instance_TIER_UNSPECIFIED {
		return apierror.InvalidArgument("The Filestore instance %s is of tier %s: Backup and DR protects Zonal, Regional and Enterprise instances only.",
			in.GetName(), tierName(in.GetTier()))
	}
	return nil
}

// bdrKey is the backend's key for a run's copy: a word, a hyphen and hex,
// as the NFS servers' keys are.
func bdrKey(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "backupdr-" + hex.EncodeToString(sum[:16])
}

// BackupDRBackup copies instance name's share out as a vault's backup kept
// as runID, and answers its size when it is taken. The instance is busy
// meanwhile, as for a backup.
func (s *Server) BackupDRBackup(ctx context.Context, name, runID string) (int64, error) {
	if err := s.noBackend(); err != nil {
		return 0, err
	}
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return 0, apierror.InvalidArgument("run ID %q", runID)
	}
	s.mu.Lock()
	r, err := s.instanceLocked(name)
	if err != nil {
		s.mu.Unlock()
		return 0, apierror.NotFound("The Filestore instance %s does not exist.", name)
	}
	if err := bdrTier(r.I); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if err := s.busyLocked(name); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		s.mu.Unlock()
		return 0, apierror.FailedPrecondition("Instance %s is %s: a backup can be made only when it is READY", name, r.I.GetState())
	}
	fs := r.I.GetFileShares()[0]
	rec := &bdrBackup{Key: bdrKey(runID), Source: name, Share: fs.GetName(), CapacityGb: fs.GetCapacityGb(),
		Tier: tierName(r.I.GetTier()), Protocol: r.I.GetProtocol().String(), Time: s.now()}
	s.busy[name] = "Backup and DR backup " + runID
	key := r.Key
	b := s.opts.Backend
	s.mu.Unlock()
	size, err := b.Backup(ctx, key, rec.Share, rec.Key, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, name)
	if err != nil {
		_ = b.DeleteBackup(context.Background(), rec.Key)
		return 0, apierror.Internal(err, "The backup of %s failed: %v", name, err)
	}
	if s.bdrBackups == nil {
		s.bdrBackups = map[string]*bdrBackup{}
	}
	s.bdrBackups[runID] = rec
	_ = s.saveLocked()
	return size, nil
}

// DeleteBackupDRBackup deletes a vault's backup's data.
func (s *Server) DeleteBackupDRBackup(ctx context.Context, runID string) error {
	s.mu.Lock()
	rec := s.bdrBackups[runID]
	delete(s.bdrBackups, runID)
	_ = s.saveLocked()
	s.mu.Unlock()
	if rec == nil || s.opts.Backend == nil {
		return nil
	}
	return s.opts.Backend.DeleteBackup(ctx, rec.Key)
}

// bdrSourceLocked checks a new instance's sourceBackupdrBackup, and
// answers the backup's data, nil when the share names none. The caller
// holds mu.
func (s *Server) bdrSourceLocked(in *filestorepb.Instance) (*bdrBackup, string, error) {
	fs := in.GetFileShares()[0]
	src := SourceBackupDR(fs)
	if src == "" {
		return nil, "", nil
	}
	if fs.GetSourceBackup() != "" {
		return nil, "", apierror.InvalidArgument("file_shares[0]: give source_backup or source_backupdr_backup, not both")
	}
	if !strings.Contains(src, "/backupVaults/") || !strings.Contains(src, "/dataSources/") || !strings.Contains(src, "/backups/") {
		return nil, "", apierror.InvalidArgument("source_backupdr_backup %q must be projects/{project_id}/locations/{location_id}/backupVaults/{backupvault_id}/dataSources/{datasource_id}/backups/{backup_id}", src)
	}
	if s.bdrAPI == nil {
		return nil, "", apierror.FailedPrecondition("the Backup and DR Service is not served by this instance: run up with backupdr in --services")
	}
	if err := bdrTier(in); err != nil {
		return nil, "", apierror.InvalidArgument("A Backup and DR backup can be restored only to a Zonal, Regional or Enterprise instance, not tier %s", tierName(in.GetTier()))
	}
	runID, err := s.bdrAPI.FilestoreBackup(src)
	if err != nil {
		return nil, "", err
	}
	rec := s.bdrBackups[runID]
	if rec == nil {
		return nil, "", apierror.FailedPrecondition("the data of Backup and DR backup %s is not on this instance", src)
	}
	if fs.GetCapacityGb() < rec.CapacityGb {
		return nil, "", apierror.InvalidArgument("The provisioned capacity of the new instance, %d GiB, must be equal to or exceed the provisioned capacity of the source instance, %d GiB", fs.GetCapacityGb(), rec.CapacityGb)
	}
	return rec, src, nil
}
