package sqladmin

// Backup locations and backup encryption (#1956).
//
// Location: a backup is kept in a location, the region or multi-region
// settings.backupConfiguration.location names for the instance's
// backups, or the one an on-demand backup's own location names
// (backupRuns.insert, backups.create, `gcloud sql backups create
// --location`). Without one it is the multi-region nearest the instance,
// as Cloud SQL's default is ("us" for us-central1). Every backup is kept
// on this host whatever its location; the location is recorded and
// answered as given. A backup in another location than the instance's
// region must be allowed by gcp.resourceLocations, or it fails ("Backups
// fail if they are in a region the organization policy does not allow";
// "Backups taken in the same region as the instance always succeed").
//
// Encryption: a backup of a CMEK instance inherits its key, and is
// wrapped with the key's latest primary version when it is taken
// ("Automated and manual backups use the latest primary key version",
// docs.cloud.google.com/sql/docs/mysql/cmek): the instance's data key is
// wrapped again for the backup, and the backup run answers
// diskEncryptionStatus.kmsKeyVersionName, the backup kmsKeyVersion. A
// backup is restored only while that version can decrypt ("If anyone
// destroys or disables this key version, all backup data encrypted using
// that key version will be permanently lost" until it is enabled again).
// instances.reencrypt's backupReencryptionConfig wraps the instance's
// backups of backupType (AUTOMATED by default) again with the key's
// primary version, newest first, at most backupLimit of them. As with the
// instance, the dump files themselves are not encrypted with the key
// (docs/cloudsql.md).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// multiRegions are Cloud SQL's backup multi-regions.
var multiRegions = map[string]bool{"us": true, "eu": true, "asia": true}

// defaultBackupLocation is the multi-region nearest region.
// UNVERIFIED (docs re-read 2026-11-19: <https://docs.cloud.google.com/sql/docs/mysql/backup-recovery/backups>):
// Google says the default is "the multi-region that is geographically
// closest"; which multi-region a region outside the US, Europe and Asia
// maps to is CloudBurrow's choice.
func defaultBackupLocation(region string) string {
	switch {
	case strings.HasPrefix(region, "europe-"), strings.HasPrefix(region, "me-"), strings.HasPrefix(region, "africa-"):
		return "eu"
	case strings.HasPrefix(region, "asia-"), strings.HasPrefix(region, "australia-"):
		return "asia"
	}
	return "us"
}

// checkBackupLocation is nil for an empty location, a region, or a
// multi-region.
func checkBackupLocation(field, loc string) error {
	if loc == "" || multiRegions[loc] || regionRE.MatchString(loc) {
		return nil
	}
	return Errorf(http.StatusBadRequest, "Invalid request: %s %q is not a region or a multi-region (us, eu, asia).", field, loc)
}

// configuredBackupLocation is settings.backupConfiguration.location.
func configuredBackupLocation(spec Spec) string {
	bc, _ := spec.Settings["backupConfiguration"].(map[string]any)
	loc, _ := bc["location"].(string)
	return loc
}

// backupPlacement is where a new backup of the instance goes: the
// location asked for, else the configured one, else the default; custom
// is whether it was chosen rather than defaulted.
func (s *Server) backupPlacement(instance, asked string) (loc string, custom bool, spec Spec) {
	s.emu.RLock()
	if s.writes != nil {
		if c := s.writes.byName[instance]; c != nil {
			spec = c.spec
		}
	}
	s.emu.RUnlock()
	switch {
	case asked != "":
		return asked, true, spec
	case configuredBackupLocation(spec) != "":
		return configuredBackupLocation(spec), true, spec
	}
	return defaultBackupLocation(spec.region()), false, spec
}

// backupLocationAllowed applies gcp.resourceLocations to a custom
// location other than the instance's region.
func backupLocationAllowed(ctx context.Context, project, loc string, spec Spec) error {
	if loc == spec.region() {
		return nil
	}
	return orgpolicy.CheckLocations(ctx, "projects/"+project, "projects/"+project, loc)
}

// backupKey wraps the instance's data key with its CryptoKey's primary
// version, for a backup taken now.
func (s *Server) backupKey(spec Spec) (wrapped []byte, version string, err error) {
	if spec.KMSKey == "" {
		return nil, "", nil
	}
	s.emu.RLock()
	k := s.kms
	s.emu.RUnlock()
	if k == nil {
		return nil, "", s.keyUsableWith(nil, spec)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dek, err := k.Decrypt(ctx, spec.KMSKey, spec.WrappedKey)
	if err != nil {
		return nil, "", keyError(spec.KMSKey, fmt.Errorf("instance %s: %w", spec.Name, err))
	}
	wrapped, version, err = k.Encrypt(ctx, spec.KMSKey, dek)
	if err != nil {
		return nil, "", keyError(spec.KMSKey, err)
	}
	return wrapped, version, nil
}

// backupKeyUsable is nil when the backup has no key or the version that
// wrapped its data key can decrypt it: a backup is restored only then.
func (s *Server) backupKeyUsable(r BackupRun) error {
	if r.KMSKey == "" {
		return nil
	}
	s.emu.RLock()
	k := s.kms
	s.emu.RUnlock()
	if k == nil {
		return Errorf(http.StatusPreconditionFailed, "The backup %s is encrypted with %s, and Cloud KMS is not enabled: "+
			"run `cloudburrow up` with kms in --services.", r.ID, r.KMSKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := k.Decrypt(ctx, r.KMSKey, r.WrappedKey); err != nil {
		return Errorf(http.StatusBadRequest, "The backup %s cannot be restored: the key version %s that encrypted it cannot be used: %v",
			r.ID, r.KMSKeyVersion, err)
	}
	return nil
}

// backupEncryptionJSON adds a backup run's key to its answer.
func backupEncryptionJSON(out map[string]any, r *BackupRun) {
	if r.KMSKey == "" {
		return
	}
	out["diskEncryptionConfiguration"] = map[string]any{"kind": "sql#diskEncryptionConfiguration", "kmsKeyName": r.KMSKey}
	out["diskEncryptionStatus"] = map[string]any{"kind": "sql#diskEncryptionStatus", "kmsKeyVersionName": r.KMSKeyVersion}
}

// backupLocation is a run's location; a run recorded before #1956 has
// none and was answered in the default region.
func backupLocation(r *BackupRun) string {
	if r.Location == "" {
		return Region
	}
	return r.Location
}

// backupReencryption is a reencrypt's backupReencryptionConfig.
type backupReencryption struct {
	BackupLimit int    `json:"backupLimit"`
	BackupType  string `json:"backupType"`
}

func parseBackupReencryption(raw *json.RawMessage) (*backupReencryption, error) {
	if raw == nil || string(*raw) == "null" {
		return nil, nil
	}
	var c backupReencryption
	if err := json.Unmarshal(*raw, &c); err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid JSON payload received: backupReencryptionConfig: %v", err)
	}
	switch c.BackupType {
	case "", "BACKUP_TYPE_UNSPECIFIED":
		// "Unknown backup type, will be defaulted to AUTOMATIC backup type".
		c.BackupType = "AUTOMATED"
	case "AUTOMATED", "ON_DEMAND":
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid value for backupReencryptionConfig.backupType: %s.", c.BackupType)
	}
	if c.BackupLimit < 0 {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: backupReencryptionConfig.backupLimit must not be negative, got %d.", c.BackupLimit)
	}
	return &c, nil
}

// rewrappedBackup is one backup's new wrapping.
type rewrappedBackup struct {
	id, version string
	wrapped     []byte
}

// rewrapBackups wraps the instance's backups that cfg selects again with
// the key's primary version: newest first, at most backupLimit (all of
// them when it is 0). A backup whose version cannot decrypt is an error,
// and nothing is changed.
// UNVERIFIED (docs re-read 2026-11-19: <https://docs.cloud.google.com/sql/docs/mysql/admin-api/rest/v1/instances/reencrypt>):
// Google documents backupLimit only as "Backup re-encryption limit"; that
// 0 means no limit, and that the newest backups are taken first, are
// CloudBurrow's.
func (s *Server) rewrapBackups(ctx context.Context, k KeyService, project, instance, key string, cfg *backupReencryption) ([]rewrappedBackup, error) {
	b := s.backups
	if b == nil {
		return nil, s.noBackups("reencrypt's backupReencryptionConfig")
	}
	b.mu.Lock()
	var runs []BackupRun
	for _, r := range b.sortedLocked(project, instance) {
		if r.Type == cfg.BackupType && r.Status == "SUCCESSFUL" && r.KMSKey == key && !r.BackupDR {
			runs = append(runs, *r)
		}
	}
	b.mu.Unlock()
	if cfg.BackupLimit > 0 && len(runs) > cfg.BackupLimit {
		runs = runs[:cfg.BackupLimit]
	}
	out := make([]rewrappedBackup, 0, len(runs))
	for _, r := range runs {
		dek, err := k.Decrypt(ctx, key, r.WrappedKey)
		if err != nil {
			return nil, keyError(key, fmt.Errorf("backup %s (key version %s): %w", r.ID, r.KMSKeyVersion, err))
		}
		wrapped, version, err := k.Encrypt(ctx, key, dek)
		if err != nil {
			return nil, keyError(key, err)
		}
		out = append(out, rewrappedBackup{id: r.ID, version: version, wrapped: wrapped})
	}
	return out, nil
}

// applyRewrapped records the backups' new wrapping.
func (s *Server) applyRewrapped(rs []rewrappedBackup) error {
	if len(rs) == 0 {
		return nil
	}
	b := s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range rs {
		if r := b.runs[w.id]; r != nil {
			r.KMSKeyVersion, r.WrappedKey = w.version, w.wrapped
		}
	}
	return b.saveLocked()
}
