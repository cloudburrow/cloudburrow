package sqladmin

// instances.restoreBackup into an instance that does not exist yet (#1922).
// A backup given by name (a Backup and DR backup's backupdrBackup, or a
// project-level backup's backup) restores to a new instance: Google's
// Backup and DR guide's "Restore to a new instance ... creates a new
// instance based on the configuration stored in your backup", and gcloud
// sql backups restore NAME --restore-instance=NEW. The new instance takes
// the source instance's settings, overridden by restoreInstanceSettings,
// whose rules are the API reference's: "You can't change the instance's
// major database version and you can only increase the disk size."
// Cloud SQL's restore guide adds that it "always sets the storage capacity
// of the target instance to the maximum value" of the two, so a smaller
// disk is raised rather than refused. The instance is made as
// instances.clone makes its target, and the backup's dump is loaded into
// it. A restoreBackupContext names a backup run by its instance, and only
// restores into an instance that exists.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// majorVersion is a databaseVersion's major version: MYSQL_8_0 for
// MYSQL_8_0_31, POSTGRES_16, SQLSERVER_2022 for SQLSERVER_2022_STANDARD.
func majorVersion(v string) string {
	p := strings.Split(v, "_")
	n := 2
	if p[0] == "MYSQL" {
		n = 3
	}
	if len(p) < n {
		return v
	}
	return strings.Join(p[:n], "_")
}

// diskGB is settings.dataDiskSizeGb as recorded (a string or a number), 0
// when unset.
func diskGB(settings map[string]any) int64 {
	switch v := settings["dataDiskSizeGb"].(type) {
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	case float64:
		return int64(v)
	}
	return 0
}

func (s *Server) restoreToNewInstance(r *http.Request, base, project, dest string) (any, error) {
	missing := Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist. CloudBurrow's instances are %s.", s.instanceNames())
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %v", err)
	}
	var body struct {
		Ctx             json.RawMessage `json:"restoreBackupContext"`
		Backup          string          `json:"backup"`
		BackupdrBackup  string          `json:"backupdrBackup"`
		RestoreSettings json.RawMessage `json:"restoreInstanceSettings"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
		}
	}
	if body.Backup == "" && body.BackupdrBackup == "" {
		// A backup run restores only into an instance that exists.
		return nil, missing
	}
	if (body.Backup != "" && body.BackupdrBackup != "") || (len(body.Ctx) > 0 && string(body.Ctx) != "null") {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: give one of restoreBackupContext, backup and backupdrBackup.")
	}
	if s.writes == nil {
		return nil, s.noWrites("restoring a backup to a new instance")
	}
	if s.backups == nil {
		return nil, s.noBackups("instances.restoreBackup")
	}
	var cp BackupRun
	if body.BackupdrBackup != "" {
		cp, err = s.backupDRRun(body.BackupdrBackup)
	} else {
		cp, err = s.backupByName(body.Backup)
	}
	if err != nil {
		return nil, err
	}
	if cp.Status != "SUCCESSFUL" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: the backup run %s is %s, not SUCCESSFUL.", cp.ID, cp.Status)
	}
	if !instanceNameRE.MatchString(dest) || len(dest) > maxInstanceName {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance name %q must be lowercase letters, digits and hyphens, "+
			"start with a letter, not end with a hyphen, and be at most %d characters in CloudBurrow.", dest, maxInstanceName)
	}
	// The backup's settings: its instance's, while it is recorded.
	spec := Spec{Tier: "db-custom-1-3840", AvailabilityType: "ZONAL"}
	s.emu.RLock()
	if c := s.writes.byName[cp.Instance]; c != nil && c.spec.Project == cp.Project {
		spec = c.spec
		if spec.Settings != nil {
			settings := make(map[string]any, len(spec.Settings))
			for k, v := range spec.Settings {
				settings[k] = v
			}
			spec.Settings = settings
		}
	} else if m, ok := s.engineLocked(cp.Instance).(*MySQL); ok {
		spec.RootPassword = m.RootPassword
	}
	s.emu.RUnlock()
	spec.DatabaseVersion = cp.DatabaseVersion
	// A restored instance stands alone: no primary, no replication.
	spec.MasterInstanceName, spec.MasterUp, spec.ReplicationPassword = "", false, ""
	spec.ReplicationStopped, spec.AwaitingPrimary = false, false
	sourceDisk := diskGB(spec.Settings)
	if len(body.RestoreSettings) > 0 && string(body.RestoreSettings) != "null" {
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body.RestoreSettings))
		b, err := decodeInstance(req)
		if err != nil {
			return nil, err
		}
		if b.DatabaseVersion != "" {
			if majorVersion(b.DatabaseVersion) != majorVersion(cp.DatabaseVersion) {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: restoreInstanceSettings.databaseVersion %s is not the backup's "+
					"major version %s: a restore cannot change the instance's major database version.", b.DatabaseVersion, majorVersion(cp.DatabaseVersion))
			}
			spec.DatabaseVersion = b.DatabaseVersion
		}
		if b.Name != "" && b.Name != dest {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: restoreInstanceSettings.name %q is not the instance restored to, %q.", b.Name, dest)
		}
		if b.MasterInstanceName != "" || b.ReplicaConfiguration != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: a restored instance cannot be a read replica.")
		}
		if err := b.unsupported(); err != nil {
			return nil, err
		}
		b.apply(&spec, false)
		if b.Region != "" {
			if !regionRE.MatchString(b.Region) {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for region: %s.", b.Region)
			}
			spec.Region = b.Region
			if b.Region == Region {
				spec.Region = ""
			}
		}
		if b.RootPassword != "" {
			spec.RootPassword = b.RootPassword
		}
		// "you can only increase the disk size": the larger of the two.
		if d := diskGB(spec.Settings); sourceDisk > 0 && d < sourceDisk {
			spec.Settings["dataDiskSizeGb"] = strconv.FormatInt(sourceDisk, 10)
		}
	}
	spec.Name, spec.Project = dest, project
	spec.CreateTime, spec.SettingsVersion = time.Now().UTC().Format(time.RFC3339Nano), 1
	if legacyMySQL(spec.DatabaseVersion) {
		return nil, legacyUnimplemented("restoring a backup to a new instance")
	}
	if !SupportedVersions[spec.DatabaseVersion] {
		return nil, unimplemented("restoring a "+spec.DatabaseVersion+" backup to a new instance", unservedVersionWhy(spec.DatabaseVersion))
	}
	if !s.engineServed(spec.DatabaseVersion) {
		return nil, Errorf(http.StatusPreconditionFailed, "Invalid request: a %s instance runs an image this instance was not started with.",
			spec.DatabaseVersion)
	}
	if !spec.MySQL() {
		spec.RootPassword = ""
	}
	if spec.Regional() && spec.HAPassword == "" {
		spec.HAPassword, spec.HAEpochs[1] = newReplicationPassword(), 1
	}
	s.emu.Lock()
	if s.engineLocked(dest) != nil || s.writes.sources[dest] != nil {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
	}
	c := &created{spec: spec, engine: s.writes.prov.Engine(spec), state: "PENDING_CREATE"}
	s.writes.byName[dest] = c
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("sqladmin: keep instance %s: %w", dest, err)
	}
	if s.conn != nil {
		if err := s.conn.addInstance(dest); err != nil {
			return nil, err
		}
	}
	op := s.pending(base, project, dest, "RESTORE_VOLUME")
	id := op["name"].(string)
	if body.BackupdrBackup != "" {
		s.logRestore(id, "RESTORE", body.BackupdrBackup, project, dest, "")
	}
	b := s.backups
	go func() {
		s.bringUp(dest, "")
		s.emu.RLock()
		state, failure := c.state, c.failure
		s.emu.RUnlock()
		if state != "RUNNABLE" {
			s.finish(id, fmt.Errorf("the restored instance's server did not start: %s", failure))
			return
		}
		s.setState(dest, "PENDING_CREATE")
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		err := b.dumper.Load(ctx, DumpTarget{Instance: dest, MySQL: spec.MySQL(), Created: true}, b.runDir(cp.ID))
		cancel()
		if err == nil {
			err = s.takePITRBase(dest)
		}
		s.emu.Lock()
		if s.writes.byName[dest] == c && c.state == "PENDING_CREATE" {
			c.state = "RUNNABLE"
			if err != nil {
				c.state, c.failure = "FAILED", err.Error()
			}
		}
		s.emu.Unlock()
		s.finish(id, err)
	}()
	return op, nil
}
