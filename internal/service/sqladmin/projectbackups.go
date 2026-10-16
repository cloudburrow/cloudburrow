package sqladmin

// The project-level Backups API (#1462): projects/{project}/backups/{id}
// names the same backups an instance's backupRuns are, with their expiry,
// and the FINAL backup instances.delete takes when enableFinalBackup is
// set, which outlives its instance until it expires.

import (
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// backupName is a backup's project-level name.
func backupName(r *BackupRun) string { return "projects/" + r.Project + "/backups/" + r.ID }

func backupJSON(base string, r *BackupRun) map[string]any {
	out := map[string]any{
		"kind": "sql#backup", "name": backupName(r), "instance": r.Instance, "type": r.Type, "state": r.Status,
		"databaseVersion": r.DatabaseVersion, "location": Region, "backupKind": "SNAPSHOT",
		"selfLink":  base + backupName(r),
		"backupRun": base + "projects/" + r.Project + "/instances/" + r.Instance + "/backupRuns/" + r.ID,
	}
	iv := map[string]any{}
	if r.StartTime != "" {
		iv["startTime"] = r.StartTime
	}
	if r.EndTime != "" {
		iv["endTime"] = r.EndTime
	}
	if len(iv) > 0 {
		out["backupInterval"] = iv
	}
	if r.Description != "" {
		out["description"] = r.Description
	}
	if r.ExpiryTime != "" {
		out["expiryTime"] = r.ExpiryTime
	}
	if r.TTLDays > 0 {
		out["ttlDays"] = strconv.FormatInt(r.TTLDays, 10)
	}
	if r.InstanceDeletionTime != "" {
		out["instanceDeletionTime"] = r.InstanceDeletionTime
	}
	if r.Error != "" {
		out["error"] = map[string]any{"kind": "sql#operationError", "code": "INTERNAL_ERROR", "message": r.Error}
	}
	return out
}

// expiry is when a backup given ttlDays or expiryTime expires, from now;
// both is an error, as on Cloud SQL.
func expiry(now time.Time, ttlDays int64, expiryTime string) (string, error) {
	switch {
	case ttlDays != 0 && expiryTime != "":
		return "", Errorf(http.StatusBadRequest, "Invalid request: give ttlDays or expiryTime, not both.")
	case ttlDays < 0 || ttlDays > 365:
		return "", Errorf(http.StatusBadRequest, "Invalid request: ttlDays must be between 1 and 365, got %d.", ttlDays)
	case ttlDays > 0:
		return now.Add(time.Duration(ttlDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano), nil
	case expiryTime != "":
		t, err := time.Parse(time.RFC3339Nano, expiryTime)
		if err != nil {
			return "", Errorf(http.StatusBadRequest, "Invalid request: expiryTime %q is not an RFC 3339 time.", expiryTime)
		}
		if !t.After(now) {
			return "", Errorf(http.StatusBadRequest, "Invalid request: expiryTime %s is in the past.", expiryTime)
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", nil
}

// projectBackups routes projects/{project}/backups[/{id}].
func (s *Server) projectBackups(r *http.Request, base, project string, rest []string) (string, any, error) {
	b := s.backups
	method := map[string]string{http.MethodGet: "sql.Backups.ListBackups", http.MethodPost: "sql.Backups.CreateBackup"}[r.Method]
	if len(rest) == 1 {
		method = map[string]string{http.MethodGet: "sql.Backups.GetBackup", http.MethodPatch: "sql.Backups.UpdateBackup",
			http.MethodDelete: "sql.Backups.DeleteBackup"}[r.Method]
	}
	if method == "" || len(rest) > 1 {
		return "sql.Backups", nil, Errorf(http.StatusNotFound, "%s %s is not a Cloud SQL Admin API method", r.Method, r.URL.Path)
	}
	if b == nil {
		return method, nil, s.noBackups(method)
	}
	switch method {
	case "sql.Backups.ListBackups":
		out, err := s.listBackups(r, base, project)
		return method, out, err
	case "sql.Backups.CreateBackup":
		out, err := s.createBackup(r, base, project)
		return method, out, err
	}
	run, err := s.backupByID(project, rest[0])
	if err != nil {
		return method, nil, err
	}
	switch method {
	case "sql.Backups.GetBackup":
		return method, backupJSON(base, &run), nil
	case "sql.Backups.UpdateBackup":
		out, err := s.updateBackup(r, base, run)
		return method, out, err
	}
	if run.Status == "RUNNING" || run.Status == "ENQUEUED" {
		return method, nil, Errorf(http.StatusConflict, "Operation failed because the backup is still being taken.")
	}
	b.mu.Lock()
	delete(b.runs, run.ID)
	err = b.saveLocked()
	b.mu.Unlock()
	if err == nil {
		err = os.RemoveAll(b.runDir(run.ID))
	}
	if err != nil {
		return method, nil, err
	}
	return method, s.done(base, project, namedEngine(run.Instance), "DELETE_BACKUP", run.Instance), nil
}

// backupByID is project's backup id; the Backup and DR Service's are its
// own (they are named by their vault), not the project's.
func (s *Server) backupByID(project, id string) (BackupRun, error) {
	b := s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.runs[id]
	if run == nil || run.Project != project || run.BackupDR {
		return BackupRun{}, Errorf(http.StatusNotFound, "The backup projects/%s/backups/%s does not exist.", project, id)
	}
	return *run, nil
}

// backupByName is the backup a Backups resource name names.
func (s *Server) backupByName(name string) (BackupRun, error) {
	p := strings.Split(name, "/")
	if len(p) != 4 || p[0] != "projects" || p[2] != "backups" || p[1] == "" || p[3] == "" {
		return BackupRun{}, Errorf(http.StatusBadRequest, "Invalid request: backup %q is not projects/{project}/backups/{backup}.", name)
	}
	return s.backupByID(p[1], p[3])
}

var backupFilterTerm = regexp.MustCompile(`^\s*(instance|type|state)\s*[:=]\s*"?([^"\s]*)"?\s*$`)

// listBackups is ListBackups: newest first, filtered by instance, type
// and state (`instance="db" AND type=FINAL`).
func (s *Server) listBackups(r *http.Request, base, project string) (any, error) {
	want := map[string]string{}
	if f := strings.TrimSpace(r.URL.Query().Get("filter")); f != "" {
		for _, term := range regexp.MustCompile(`\s+AND\s+`).Split(f, -1) {
			m := backupFilterTerm.FindStringSubmatch(term)
			if m == nil {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: filter %q: CloudBurrow filters backups by instance, type and state, joined by AND.", f)
			}
			want[m[1]] = m[2]
		}
	}
	b := s.backups
	b.mu.Lock()
	items := []any{}
	for _, run := range b.sortedLocked(project, want["instance"]) {
		if run.BackupDR || (want["type"] != "" && run.Type != want["type"]) || (want["state"] != "" && run.Status != want["state"]) {
			continue
		}
		items = append(items, backupJSON(base, run))
	}
	b.mu.Unlock()
	start := 0
	if t := r.URL.Query().Get("pageToken"); t != "" {
		var err error
		if start, err = strconv.Atoi(t); err != nil || start < 0 || start > len(items) {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: pageToken %q is not one this server returned.", t)
		}
	}
	out := map[string]any{}
	end := len(items)
	if n, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && n > 0 && start+n < end {
		end = start + n
		out["nextPageToken"] = strconv.Itoa(end)
	}
	out["backups"] = items[start:end]
	return out, nil
}

// createBackup is CreateBackup: an ON_DEMAND backup of body.instance, as
// backupRuns.insert takes, with the expiry the body gives.
func (s *Server) createBackup(r *http.Request, base, project string) (any, error) {
	var body struct {
		Instance    string `json:"instance"`
		Description string `json:"description"`
		Location    string `json:"location"`
		TTLDays     int64  `json:"ttlDays,string"`
		ExpiryTime  string `json:"expiryTime"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Instance == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: backup.instance is required.")
	}
	if body.Location != "" && body.Location != Region {
		return nil, unimplemented("a backup in location "+body.Location, "CloudBurrow keeps every backup in its one region, "+Region)
	}
	exp, err := expiry(s.Clock(), body.TTLDays, body.ExpiryTime)
	if err != nil {
		return nil, err
	}
	e := s.engineIn(project, body.Instance)
	if e == nil {
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL instance %s does not exist in project %s.", body.Instance, project)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	op, id, err := s.takeBackupRun(base, project, e, "ON_DEMAND", body.Description, false)
	if err != nil {
		return nil, err
	}
	b := s.backups
	b.mu.Lock()
	if run := b.runs[id]; run != nil {
		run.ExpiryTime, run.TTLDays = exp, body.TTLDays
		_ = b.saveLocked()
	}
	b.mu.Unlock()
	return op, nil
}

// updateBackup is UpdateBackup: the description of any backup, and the
// expiry of a FINAL one, as Cloud SQL allows.
func (s *Server) updateBackup(r *http.Request, base string, run BackupRun) (any, error) {
	var body struct {
		Description *string `json:"description"`
		TTLDays     int64   `json:"ttlDays,string"`
		ExpiryTime  string  `json:"expiryTime"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	mask := r.URL.Query().Get("updateMask")
	if mask == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: updateMask is required.")
	}
	var desc *string
	var ttl int64
	var expTime string
	for _, f := range strings.Split(mask, ",") {
		switch strings.TrimSpace(f) {
		case "description":
			d := ""
			if body.Description != nil {
				d = *body.Description
			}
			desc = &d
		case "ttl_days", "ttlDays":
			ttl = body.TTLDays
			if ttl == 0 {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: ttlDays is in updateMask but not set.")
			}
		case "expiry_time", "expiryTime":
			expTime = body.ExpiryTime
			if expTime == "" {
				return nil, Errorf(http.StatusBadRequest, "Invalid request: expiryTime is in updateMask but not set.")
			}
		default:
			return nil, Errorf(http.StatusBadRequest, "Invalid request: updateMask field %q cannot be updated; a backup's description, ttl_days and expiry_time can.", f)
		}
	}
	var exp string
	if ttl != 0 || expTime != "" {
		if run.Type != "FINAL" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: the retention of a %s backup cannot be changed; only a FINAL backup's ttl_days and expiry_time can.", run.Type)
		}
		var err error
		if exp, err = expiry(s.Clock(), ttl, expTime); err != nil {
			return nil, err
		}
	}
	b := s.backups
	b.mu.Lock()
	cur := b.runs[run.ID]
	if cur == nil {
		b.mu.Unlock()
		return nil, Errorf(http.StatusNotFound, "The backup %s does not exist.", backupName(&run))
	}
	if desc != nil {
		cur.Description = *desc
	}
	if exp != "" {
		cur.ExpiryTime, cur.TTLDays = exp, ttl
	}
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.done(base, run.Project, namedEngine(run.Instance), "UPDATE_BACKUP", run.Instance), nil
}

// sweepExpiredBackups deletes every backup whose expiryTime has passed on
// the retention clock.
func (s *Server) sweepExpiredBackups() {
	b := s.backups
	if b == nil {
		return
	}
	now := s.Clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for id, r := range b.runs {
		if r.ExpiryTime == "" || r.BackupDR {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, r.ExpiryTime); err == nil && !t.After(now) {
			delete(b.runs, id)
			_ = os.RemoveAll(b.runDir(id))
			changed = true
		}
	}
	if changed {
		_ = b.saveLocked()
	}
}

// finalBackup is instances.delete's enableFinalBackup and its
// finalBackup* parameters, read from the query.
type finalBackup struct {
	enabled     bool
	description string
	expiry      string
	ttlDays     int64
}

func (s *Server) finalBackupOf(r *http.Request) (finalBackup, error) {
	q := r.URL.Query()
	fb := finalBackup{description: q.Get("finalBackupDescription")}
	fb.enabled, _ = strconv.ParseBool(q.Get("enableFinalBackup"))
	if v := q.Get("finalBackupTtlDays"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fb, Errorf(http.StatusBadRequest, "Invalid request: finalBackupTtlDays %q is not a number.", v)
		}
		fb.ttlDays = n
	}
	if !fb.enabled {
		if fb.ttlDays != 0 || q.Get("finalBackupExpiryTime") != "" || fb.description != "" {
			return fb, Errorf(http.StatusBadRequest, "Invalid request: the finalBackup parameters need enableFinalBackup.")
		}
		return fb, nil
	}
	if s.backups == nil {
		return fb, s.noBackups("a final backup")
	}
	if fb.ttlDays == 0 && q.Get("finalBackupExpiryTime") == "" {
		// Cloud SQL keeps a final backup 30 days unless told otherwise.
		fb.ttlDays = 30
	}
	exp, err := expiry(s.Clock(), fb.ttlDays, q.Get("finalBackupExpiryTime"))
	fb.expiry = exp
	return fb, err
}

// takeFinalBackup dumps e into a FINAL backup before its instance is
// removed; it is kept when the instance is gone.
func (s *Server) takeFinalBackup(project string, e Engine, fb finalBackup) error {
	b := s.backups
	now := time.Now().UTC().Format(time.RFC3339Nano)
	b.mu.Lock()
	run := &BackupRun{ID: b.newIDLocked(), Project: project, Instance: e.Instance(), DatabaseVersion: e.DatabaseVersion(),
		Type: "FINAL", Description: fb.description, Status: "RUNNING", EnqueuedTime: now, StartTime: now, WindowStartTime: now,
		ExpiryTime: fb.expiry, TTLDays: fb.ttlDays}
	b.runs[run.ID] = run
	_ = b.saveLocked()
	b.mu.Unlock()
	err := s.dump(s.target(e), run.ID)
	b.mu.Lock()
	defer b.mu.Unlock()
	run.EndTime = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		run.Status, run.Error = "FAILED", err.Error()
		_ = os.RemoveAll(b.runDir(run.ID))
	} else {
		run.Status = "SUCCESSFUL"
	}
	_ = b.saveLocked()
	return err
}

// engineIn is project's instance name, nil when it has none: an `up`
// server is every project's, a created instance its own project's.
func (s *Server) engineIn(project, name string) Engine {
	e := s.engine(name)
	if e == nil || s.createdNamed(project, name) != nil {
		return e
	}
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes != nil && s.writes.byName[name] != nil {
		return nil // another project's
	}
	return e
}
