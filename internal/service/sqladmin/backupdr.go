package sqladmin

// The Backup and DR Service's side of Cloud SQL (#1433). The service
// (internal/service/backupdr) enrolls an instance through a backup plan
// association and takes its backups here, as backup runs marked as its own;
// instances.pointInTimeRestore and instances.restoreBackup with
// backupdrBackup resolve its datasources and backups back to the instance
// and the run through BackupDR.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"time"
)

// backupDRBackupRE is a Backup and DR backup's name.
var backupDRBackupRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/backupVaults/[^/]+/dataSources/[^/]+/backups/[^/]+$`)

// BackupDR resolves the Backup and DR Service's names for Cloud SQL.
type BackupDR interface {
	// DataSourceInstance is the project and instance a datasource
	// (projects/…/locations/…/backupVaults/…/dataSources/…) protects.
	DataSourceInstance(datasource string) (project, instance string, err error)
	// BackupRunID is the backup run a Backup and DR backup
	// (…/dataSources/…/backups/…) holds.
	BackupRunID(backup string) (string, error)
}

// SetBackupDR serves pointInTimeRestore and restoreBackup's backupdrBackup
// through b. Without it they answer 501.
func (s *Server) SetBackupDR(b BackupDR) {
	s.emu.Lock()
	s.backupdr = b
	s.emu.Unlock()
}

func (s *Server) backupDR() BackupDR {
	s.emu.RLock()
	defer s.emu.RUnlock()
	return s.backupdr
}

// InstanceInfo is what the Backup and DR Service records of an instance.
type InstanceInfo struct {
	DatabaseVersion string
	Tier            string
	CreateTime      string
	Region          string
}

// LookupInstance is project's instance, if it is served.
func (s *Server) LookupInstance(project, instance string) (InstanceInfo, bool) {
	e := s.engine(instance)
	if e == nil {
		return InstanceInfo{}, false
	}
	info := InstanceInfo{DatabaseVersion: e.DatabaseVersion(), Tier: "db-custom-1-3840", Region: s.regionOf(instance)}
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes != nil {
		if c := s.writes.byName[instance]; c != nil {
			if c.spec.Project != project {
				return InstanceInfo{}, false
			}
			info.Tier, info.CreateTime = c.spec.Tier, c.spec.CreateTime
		}
	}
	return info, true
}

// TakeBackupDRBackup takes a backup of project's instance for the Backup
// and DR Service, returning its run's ID and a channel closed, with the
// run's final record available from BackupDRRun, when it is taken.
func (s *Server) TakeBackupDRBackup(project, instance, description string) (string, <-chan struct{}, error) {
	if s.backups == nil {
		return "", nil, s.noBackups("a Backup and DR backup")
	}
	if _, ok := s.LookupInstance(project, instance); !ok {
		return "", nil, Errorf(http.StatusNotFound, "The Cloud SQL instance %s does not exist in project %s.", instance, project)
	}
	if err := s.busy(instance); err != nil {
		return "", nil, err
	}
	_, id, err := s.takeBackupRun("", project, s.engine(instance), "ON_DEMAND", description, true)
	if err != nil {
		return "", nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if r, ok := s.BackupDRRun(id); !ok || (r.Status != "RUNNING" && r.Status != "ENQUEUED") {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return id, done, nil
}

// BackupDRRun is a Backup and DR backup run's record.
func (s *Server) BackupDRRun(id string) (BackupRun, bool) {
	if s.backups == nil {
		return BackupRun{}, false
	}
	b := s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.runs[id]
	if r == nil || !r.BackupDR {
		return BackupRun{}, false
	}
	return *r, true
}

// DeleteBackupDRRun deletes a Backup and DR backup run and its dump.
func (s *Server) DeleteBackupDRRun(id string) error {
	if _, ok := s.BackupDRRun(id); !ok {
		return nil
	}
	b := s.backups
	b.mu.Lock()
	delete(b.runs, id)
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return os.RemoveAll(b.runDir(id))
}

// backupDRRun resolves restoreBackup's backupdrBackup to its run.
func (s *Server) backupDRRun(name string) (BackupRun, error) {
	bdr := s.backupDR()
	if bdr == nil {
		return BackupRun{}, unimplemented("restoring a Backup and DR backup", "the Backup and DR Service is not served by this "+
			"instance; start it with --services backupdr (docs/backupdr.md)")
	}
	if !backupDRBackupRE.MatchString(name) {
		return BackupRun{}, Errorf(http.StatusBadRequest, "Invalid request: backupdrBackup %q is not a Backup and DR backup: the format is "+
			"projects/{project}/locations/{location}/backupVaults/{backupvault}/dataSources/{datasource}/backups/{backup}.", name)
	}
	id, err := bdr.BackupRunID(name)
	if err != nil {
		return BackupRun{}, Errorf(http.StatusNotFound, "%s", err.Error())
	}
	run, ok := s.BackupDRRun(id)
	if !ok {
		return BackupRun{}, Errorf(http.StatusNotFound, "The Backup and DR backup %s has no data.", name)
	}
	if run.Status != "SUCCESSFUL" {
		return BackupRun{}, Errorf(http.StatusBadRequest, "Invalid request: the Backup and DR backup %s is %s, not SUCCESSFUL.", name, run.Status)
	}
	return run, nil
}

// cloneRequest is an instances.clone request body, for the paths that
// reuse it.
func cloneRequest(dest, pointInTime string) *http.Request {
	body, _ := json.Marshal(map[string]any{"cloneContext": map[string]any{
		"destinationInstanceName": dest, "pointInTime": pointInTime}})
	r, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	return r
}
